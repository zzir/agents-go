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
// left the context as it was — retrying an identical request would fail
// identically, and spending the retry budget on that is worse than reporting
// the overflow.
func (r *runner) recoverOverflow(ctx context.Context, err error) ([]InputItem, bool) {
	r.log.Warn(ctx, "context overflow; compacting and retrying the turn",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagContextOverflow, err, nil)

	// Flush before rebuilding. The save point drains the injection queue AFTER
	// its own write, so a steer taken there is still only in memory — and the
	// retry's context comes from the log, with the in-flight items thrown away.
	// Recovering without this hands the model a conversation the caller's words
	// never reached, while the next write past their mark still counts them
	// delivered. The boundary rules are unchanged: a batch ending in a call
	// without its output stays held back, and the injection commits only once a
	// write has genuinely covered it.
	if perr := r.persistSessionItems(ctx); perr != nil {
		// The rebuilt context would be missing whatever failed to land. The
		// overflow itself is what the run reports; this is why it was not
		// recovered from.
		r.log.Warn(ctx, "overflow recovery abandoned: the session write failed",
			slog.String("error", perr.Error()))
		return nil, false
	}

	compacted, did, cerr := r.recompactAtSavePoint(ctx)
	if cerr == nil && did {
		return compacted, true
	}
	if cerr != nil {
		return nil, false
	}
	// No run-level Compactor applied (none configured, or it stood aside for a
	// self-compacting storage). A session.CompactionAware storage gets a FORCED pass:
	// its own trigger normally decides when to compact, but an overflow is the
	// one moment that question has already been answered — by the provider.
	return r.recoverOverflowViaStorage(ctx)
}

// recoverOverflowViaStorage runs a forced RunCompaction on a session.CompactionAware
// storage and rebuilds the turn's context from the session, reporting whether
// the pass is worth a retry: the history has to have changed — the Compactor
// path's rule — and, since this path's pass can be abandoned mid-flight, it has
// to have changed without growing.
func (r *runner) recoverOverflowViaStorage(ctx context.Context) ([]InputItem, bool) {
	sess := r.opts.Conversation.Session
	if sess == nil {
		return nil, false
	}
	cs, ok := sess.Storage().(session.CompactionAware)
	if !ok {
		return nil, false
	}
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
	// Different, and no longer than it was. Length alone is not the test — a
	// pass may return the same COUNT with shorter content, one summary standing
	// in for one entry — but difference alone is not either: a rewriting storage
	// guards its replacement against concurrent appends and ABANDONS the pass
	// when the log moved under it, and the append that made it abandon is itself
	// enough to leave `after` unequal to `before`. Reading that as progress buys
	// a retry of a context that only grew.
	if len(after) > len(before) || !changedEntries(before, after) {
		return nil, false
	}
	history, err := session.ProjectEntries(after, r.opts.Conversation.Projectors)
	if err != nil {
		return nil, false
	}
	// Scrub exactly as the save-point rebuild does: a fold can orphan a tool
	// output whose call is gone, which the Responses API rejects.
	return normalizeStoredInput(history), true
}
