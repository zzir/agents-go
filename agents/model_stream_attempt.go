package agents

import "iter"

// streamAttempt is the outcome of deliverStreamAttempt: one inner stream
// delivered to a consumer with pre-commit events held back. It lets
// NewRetryModel and NewFallbackModel share one commit rule (decisions §5.16).
type streamAttempt struct {
	// committed reports that output reached the consumer, committing this attempt.
	committed bool
	// err is the stream's failure; nil means it finished cleanly.
	err error
	// stopped reports that the consumer returned false mid-delivery.
	stopped bool
	// pending holds an uncommitted attempt's held-back events; the caller flushes
	// them if this attempt is the last word, or drops them when another supersedes it.
	pending []*ResponseStreamEvent
}

// deliverStreamAttempt consumes one inner stream for a retry/fallback decorator,
// holding back lifecycle and failure events until output commits it (decisions §5.16).
func deliverStreamAttempt(
	seq iter.Seq2[*ResponseStreamEvent, error],
	yield func(*ResponseStreamEvent, error) bool,
) streamAttempt {
	var a streamAttempt
	for ev, err := range seq {
		if err != nil {
			a.err = err
			return a
		}
		if ev == nil {
			continue
		}
		if !a.committed && (streamLifecycleEvent(ev.Type) || streamFailureEvent(ev.Type)) {
			a.pending = append(a.pending, ev)
			continue
		}
		if !a.committed {
			a.committed = true
			if !flushStreamEvents(a.pending, yield) {
				a.stopped = true
				return a
			}
			a.pending = nil
		}
		if !yield(ev, nil) {
			a.stopped = true
			return a
		}
	}
	return a
}

// flushStreamEvents delivers buffered events in order; false means the
// consumer stopped.
func flushStreamEvents(events []*ResponseStreamEvent, yield func(*ResponseStreamEvent, error) bool) bool {
	for _, ev := range events {
		if !yield(ev, nil) {
			return false
		}
	}
	return true
}
