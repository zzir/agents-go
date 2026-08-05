package agents

import (
	"context"
	"log/slog"
	"strings"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// OverflowPolicy decides what happens when a model call fails because the
// context did not fit.
//
// It is the reactive half of context management: compaction predicts, this
// reacts. A prediction is an estimate — a token count the SDK guessed, against
// a window the provider never states exactly — so it will sometimes be wrong,
// and the failure it misses is one the run cannot otherwise recover from.
type OverflowPolicy struct {
	// MaxRetries bounds "compact, then try this turn again". Zero disables the
	// behavior and the overflow error is returned as-is.
	MaxRetries int
}

// DetectContextOverflow reports whether a failed model call was the context
// not fitting.
//
// It matches on the message rather than a typed error because that is all the
// provider gives: a context overflow arrives as a 400 with prose in it. The
// alternative — treating every 400 as an overflow — would compact and retry
// after a malformed request, hiding a bug behind a shrinking conversation. A
// response that ARRIVED is never an overflow: a truncated one is a different
// problem with a different fix (spec §2.7e), since its input fit and
// compacting the input does not raise the output cap that cut it off. A
// backend that reports overflow in the body surfaces it as an error carrying
// one of these markers (the anthropic adapter does).
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
		// Anthropic's two shapes: a 400 whose message says the prompt did not
		// fit, and a response that ARRIVED but stopped with
		// stop_reason=model_context_window_exceeded — which the anthropic
		// adapter surfaces as an error carrying that marker, because a resend
		// without compacting would stop at the same wall.
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
// It compacts from the SESSION rather than the in-flight items, for the same
// reason the save point does: the log is the truth, and a projection of it
// cannot fall out of step with what was stored. It reports false when the pass
// left the context no smaller — retrying an identical request would fail
// identically, and spending the retry budget on that is worse than reporting
// the overflow — and false as well when the session write the rebuild depends
// on fails, since a context rebuilt without it would be missing whatever never
// landed.
//
// The two recovery paths write the session at OPPOSITE moments, which is why
// the choice between them is made here rather than by trying one and falling
// through to the other. A Compactor reads the log and returns a projection of
// it, so the turn's in-flight items have to be in the log before the pass runs.
// A self-compacting storage may REPLACE the log with what its own pass
// produced, so anything written before it is exactly what the replacement
// erases; that path writes afterwards. Falling through cannot express both.
func (r *runner) recoverOverflow(ctx context.Context, err error) ([]InputItem, bool) {
	r.log.Warn(ctx, "context overflow; compacting and retrying the turn",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagContextOverflow, err, nil)

	sess := r.opts.Conversation.Session
	if sess != nil {
		if cs, ok := sess.Storage().(session.CompactionAware); ok {
			// The storage compacts itself, so a run-level Compactor stands
			// aside (compactContext) and a save-point pass could only report
			// that nothing changed. The storage gets a FORCED pass instead: its
			// own trigger normally decides when to compact, but an overflow is
			// the one moment that question has already been answered — by the
			// provider.
			return r.recoverOverflowViaStorage(ctx, sess, cs)
		}
	}
	if sess == nil || !r.opts.Compaction.active(CompactAtSavePoint) {
		// Nothing here can shrink the context, so there is no recovery to
		// prepare for. Writing the turn anyway would mark a drained injection
		// delivered on the way to reporting the overflow — paying a recovery's
		// price for a recovery that was never available.
		return nil, false
	}

	// Flush before rebuilding. The save point drains the injection queue AFTER
	// its own write, so a steer taken there is still only in memory — and the
	// retry's context comes from the log, with the in-flight items thrown away.
	// Recovering without this hands the model a conversation the caller's words
	// never reached, while the next write past their mark still counts them
	// delivered. The boundary rules are unchanged: a batch ending in a call
	// without its output stays held back, and the injection commits only once a
	// write has genuinely covered it.
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
// reports the overflow instead — and the write error, which is the reason it
// was never recovered from, would otherwise live in the log alone. Diagnostics
// ride out on RunError.Diagnostics, which is how the caller gets to see it.
func (r *runner) abandonRecovery(ctx context.Context, err error) {
	r.log.Warn(ctx, "overflow recovery abandoned: the session write failed",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagCompactionFailed, err, map[string]any{"point": "overflow_recovery"})
}

// recoverOverflowViaStorage runs a forced RunCompaction on a
// session.CompactionAware storage, then writes the turn and rebuilds its
// context from the session.
//
// The pass runs FIRST. A self-compacting storage may answer with a replacement
// rather than a decision — the server-side compact API does, and it builds that
// replacement from its own response chain, which nothing produced locally is
// on. A write made before the pass is therefore a write the pass erases:
// injected input taken at the save point would be stored, counted delivered by
// that very write, and then deleted, with nothing left in flight to roll back.
// Writing afterwards leaves the turn standing on top of the compacted history.
//
// It reports whether the pass is worth a retry: the context has to have come
// back strictly smaller than the one that overflowed (see contextSize).
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
		// A continuation taken at the final output is appended and stored
		// before the turn that overflows, so the log can already hold items no
		// model call ever saw. Recovery costs a bigger compact request when it
		// does — and a request that fails is loud, where a replacement built
		// without them is not.
		OffChainItems: hasOffChainItems(r.sessionItems),
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
	// Read once more: the write just added the turn's items — and any injected
	// input the save point drained — on top of the compacted history, and they
	// are as much a part of the retry's context as the compacted part is.
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
// model: every entry's stored body, summed.
//
// It is what decides whether a forced pass earned its retry, in place of the
// entry count and in place of "did anything change". The count cannot decide
// it, because the read is windowed (Conversation.Settings.Limit) and a
// saturated window hides growth perfectly: a storage that guards its
// replacement against concurrent appends ABANDONS the pass when the log moved
// under it, and the one appended entry that made it abandon pushes the oldest
// entry out of the window, so the read comes back the same LENGTH — a context
// that grew, read as one that held still. "Did anything change" cannot decide
// it either, and for the same reason: that append is exactly what made it
// different.
//
// Bytes answer both, and keep the case a count was there to allow — the same
// number of entries with shorter content, one summary standing in for one
// entry, is a real compaction. An unchanged history weighs exactly what it
// weighed, so demanding strictly less rules the no-op out on its own.
//
// It is a proxy for tokens, and a deliberately conservative one: a pass whose
// result does not weigh less costs a retry the run would otherwise have spent
// on a request that already failed once.
func contextSize(entries []session.Entry) int {
	n := 0
	for _, e := range entries {
		n += len(e.Item) + len(e.Payload)
	}
	return n
}
