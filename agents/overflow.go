package agents

import (
	"context"
	"log/slog"
	"strings"
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

	// Detect classifies a failed model call as a context overflow. Nil uses
	// DetectContextOverflow.
	Detect func(err error, resp *ModelResponse) bool
}

// DetectContextOverflow reports whether a failed model call was the context
// not fitting.
//
// It matches on the message rather than a typed error because that is all the
// provider gives: a context overflow arrives as a 400 with prose in it. The
// alternative — treating every 400 as an overflow — would compact and retry
// after a malformed request, hiding a bug behind a shrinking conversation.
func DetectContextOverflow(err error, _ *ModelResponse) bool {
	// The response is part of the signature because a detector may need it —
	// a backend that reports overflow in the body rather than as an error. The
	// default does not: a response that ARRIVED is not an overflow, and a
	// truncated one is a different problem with a different fix (spec §2.7e),
	// since its input fit and compacting the input does not raise the output
	// cap that cut it off.
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"context_length_exceeded",
		"exceeds the context window",
		"maximum context length",
		"reduce the length of the messages",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isOverflow applies the policy's detector.
func (p OverflowPolicy) isOverflow(err error, resp *ModelResponse) bool {
	if p.MaxRetries <= 0 {
		return false
	}
	detect := p.Detect
	if detect == nil {
		detect = DetectContextOverflow
	}
	return detect(err, resp)
}

// recoverOverflow compacts and reports whether the turn is worth retrying.
//
// It compacts from the SESSION rather than the in-flight items, for the same
// reason the save point does: the log is the truth, and a projection of it
// cannot fall out of step with what was stored. It reports false when nothing
// was dropped — retrying an identical request would fail identically, and
// spending the retry budget on that is worse than reporting the overflow.
func (r *runner) recoverOverflow(ctx context.Context, err error) ([]TResponseInputItem, bool) {
	r.log.Warn(ctx, "context overflow; compacting and retrying the turn",
		slog.String("error", err.Error()))
	RecordDiagnostic(ctx, DiagContextOverflow, err, nil)

	compacted, did, cerr := r.recompactAtSavePoint(ctx)
	if cerr != nil || !did {
		return nil, false
	}
	return compacted, true
}
