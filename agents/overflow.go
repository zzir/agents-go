package agents

import (
	"context"
	"log/slog"
	"strings"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// OverflowPolicy decides what happens when a model call fails because the
// context did not fit. It is the reactive half of context management —
// compaction predicts, this reacts when the prediction was wrong.
type OverflowPolicy struct {
	// MaxRetries bounds "compact, then try this turn again". Zero disables the
	// behavior and the overflow error is returned as-is.
	MaxRetries int
}

// DetectContextOverflow reports whether a failed model call was the context
// not fitting.
//
// It matches on the message, not a typed error, because that is all the provider
// gives — a 400 with prose in it — and treating every 400 as overflow would hide
// a malformed request behind a shrinking conversation. A response that ARRIVED
// is never an overflow: a truncated one is a different problem with a different
// fix (spec §2.7e). A backend that reports overflow in the body surfaces it as
// an error carrying one of these markers (the anthropic adapter does).
func DetectContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"context_length_exceeded",
		"exceeds the context window",
		"maximum context length",
		"reduce the length of the messages",
		// Anthropic's two shapes: a 400 saying the prompt did not fit, and an
		// arrived response stopped with stop_reason=model_context_window_exceeded,
		// which the adapter surfaces as an error carrying this marker.
		"prompt is too long",
		"model_context_window_exceeded",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isOverflow reports whether the policy is on and the error is an overflow.
func (p OverflowPolicy) isOverflow(err error) bool {
	return p.MaxRetries > 0 && DetectContextOverflow(err)
}

// recoverOverflow compacts and reports whether the turn is worth retrying.
//
// It compacts from the session, not the in-flight items: the log is the truth,
// and a projection of it cannot fall out of step with what was stored. It
// reports false when the pass left the context no smaller, or when the session
// write the rebuild depends on fails.
//
// The two recovery paths write the session at OPPOSITE moments, which is why the
// path is chosen up front. A Compactor needs the turn in the log before it runs;
// a self-compacting storage may REPLACE the log, so its write comes after.
func (r *runner) recoverOverflow(ctx context.Context, err error) ([]InputItem, bool) {
	r.log.Warn(ctx, "context overflow; compacting and retrying the turn",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagContextOverflow, err, nil)

	sess := r.opts.Conversation.Session
	if sess != nil {
		if cs, ok := sess.Storage().(session.CompactionAware); ok {
			// The storage compacts itself, so it gets a FORCED pass: its own
			// trigger normally decides when to compact, but overflow is the one
			// moment the provider has already answered that.
			return r.recoverOverflowViaStorage(ctx, sess, cs)
		}
	}
	if sess == nil || !r.opts.Compaction.active(CompactAtSavePoint) {
		// Nothing here can shrink the context, so there is no recovery to prepare
		// for — and writing the turn anyway would mark a drained injection
		// delivered while reporting an overflow it cannot fix.
		return nil, false
	}

	// Flush before rebuilding: the retry's context comes from the log, so an
	// injected steer still only in memory would never reach the model while a
	// later write still counts it delivered. Boundary rules are unchanged — a
	// batch ending in a call without its output stays held back.
	if perr := r.persistSessionItems(ctx); perr != nil {
		r.abandonRecovery(ctx, perr)
		return nil, false
	}
	compacted, did, cerr := r.recompactAtSavePoint(ctx)
	if cerr != nil || !did {
		return nil, false
	}
	return compacted, true
}

// abandonRecovery reports a session write that killed an overflow recovery.
//
// The rebuilt context would be missing whatever failed to land, so the run
// reports the overflow instead. The write error rides out on
// RunError.Diagnostics, which is how the caller sees it.
func (r *runner) abandonRecovery(ctx context.Context, err error) {
	r.log.Warn(ctx, "overflow recovery abandoned: the session write failed",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagCompactionFailed, err, map[string]any{"point": "overflow_recovery"})
}

// recoverOverflowViaStorage runs a forced RunCompaction on a
// session.CompactionAware storage, then writes the turn and rebuilds its
// context from the session.
//
// The pass runs FIRST: a self-compacting storage may answer with a replacement
// built from its own response chain, so a write made before it is a write the
// pass erases. Writing afterwards leaves the turn on top of the compacted
// history. It reports a retry only when the context came back strictly smaller
// (see contextSize).
func (r *runner) recoverOverflowViaStorage(ctx context.Context, sess *session.Session, cs session.CompactionAware) ([]InputItem, bool) {
	cur := session.Cursor{Limit: -session.ResolveLimit(r.opts.Conversation.Settings)}
	before, err := sess.ContextEntries(ctx, cur)
	if err != nil {
		return nil, false
	}
	var cspan *tracing.SpanHandle
	cerr := cs.RunCompaction(ctx, session.CompactionArgs{
		Force:      true,
		ResponseID: r.lastResponseID,
		Store:      r.lastStore,
		// The log can already hold items no model call saw — a continuation
		// stored before this turn, a truncated read window, a handoff filter's
		// drop — so this forces the safe compact-from-history path.
		OffChainItems: r.offChainItems(),
		StartSpan: func() *tracing.SpanHandle {
			cspan = r.trace.StartCompactionSpan(r.agentParentID())
			return cspan
		},
	})
	if cerr != nil && cspan == nil {
		// Failed before the storage opened the span; open one so the error is
		// still visible on the trace (mirrors compactAfterRun).
		cspan = r.trace.StartCompactionSpan(r.agentParentID())
	}
	if cspan != nil {
		if cerr != nil {
			cspan.SetError(cerr.Error(), nil)
		}
		cspan.Finish()
	}
	if cerr != nil {
		return nil, false
	}
	after, err := sess.ContextEntries(ctx, cur)
	if err != nil {
		return nil, false
	}
	if contextSize(after) >= contextSize(before) {
		return nil, false
	}
	// The pass landed, so the turn can be written on top of what it produced.
	if perr := r.persistSessionItems(ctx); perr != nil {
		r.abandonRecovery(ctx, perr)
		return nil, false
	}
	// Read once more: the write added the turn's items (and any injected input
	// the save point drained) on top of the compacted history, and they belong
	// in the retry's context too.
	rebuilt, err := sess.ContextEntries(ctx, cur)
	if err != nil {
		return nil, false
	}
	history, err := session.ProjectEntries(rebuilt, r.opts.Conversation.Projectors)
	if err != nil {
		return nil, false
	}
	// Scrub exactly as the save-point rebuild does: a fold can orphan a tool
	// output whose call is gone, which the Responses API rejects.
	return normalizeStoredInput(history), true
}

// contextSize is the byte weight of what a set of entries puts in front of the
// model: every entry's stored body, summed. It decides whether a forced pass
// earned its retry — weight, not entry count (a windowed read hides growth from
// both; spec §2.5g).
func contextSize(entries []session.Entry) int {
	n := 0
	for _, e := range entries {
		n += len(e.Item) + len(e.Payload)
	}
	return n
}
