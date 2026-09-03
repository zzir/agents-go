package agents

import (
	"context"
	"log/slog"
	"strings"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// OverflowPolicy decides what happens when a model call fails because the
// context did not fit — the reactive half of context management (spec §2.5g).
type OverflowPolicy struct {
	// MaxRetries bounds "compact, then try this turn again". Zero disables the
	// behavior and the overflow error is returned as-is.
	MaxRetries int
}

// DetectContextOverflow reports whether a failed model call was the context
// not fitting. It matches the provider's message — all an overflow arrives as
// — and a response that ARRIVED is never an overflow (spec §2.5g, §2.7e).
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
		// Anthropic's two shapes: a 400, and an adapter-surfaced stop reason.
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

// recoverOverflow compacts from the session and reports whether the turn is
// worth retrying; the path is chosen up front — decisions §5.52.
func (r *runner) recoverOverflow(ctx context.Context, err error) ([]InputItem, bool) {
	r.log.Warn(ctx, "context overflow; compacting and retrying the turn",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagContextOverflow, err, nil)

	sess := r.opts.Conversation.Session
	if sess != nil {
		if cs, ok := sess.Storage().(session.CompactionAware); ok {
			// A self-compacting storage gets a FORCED pass: the provider already decided.
			return r.recoverOverflowViaStorage(ctx, sess, cs)
		}
	}
	if sess == nil || !r.opts.Compaction.active(CompactAtSavePoint) {
		// Nothing here can shrink the context, so there is no recovery to
		// prepare for; writing the turn anyway would mark a steer delivered.
		return nil, false
	}

	// Flush before rebuilding: the retry's context comes from the log, and a
	// steer still only in memory would never reach the model (spec §2.5g).
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

// abandonRecovery reports a session write that killed an overflow recovery;
// the run reports the overflow and the write error rides on Diagnostics.
func (r *runner) abandonRecovery(ctx context.Context, err error) {
	r.log.Warn(ctx, "overflow recovery abandoned: the session write failed",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagCompactionFailed, err, map[string]any{"point": "overflow_recovery"})
}

// recoverOverflowViaStorage runs a forced RunCompaction on a CompactionAware
// storage, then writes the turn and rebuilds from the session — decisions §5.52.
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
		// Forces the safe compact-from-history path — see offChainItems.
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
	// Read once more: the write put the turn's items (and drained injections)
	// on top of the compacted history, and they belong in the retry too.
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

// contextSize is the byte weight of what entries put in front of the model;
// weight, not count, decides whether a forced pass earned its retry (spec §2.5g).
func contextSize(entries []session.Entry) int {
	n := 0
	for _, e := range entries {
		n += len(e.Item) + len(e.Payload)
	}
	return n
}
