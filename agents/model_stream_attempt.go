package agents

import "iter"

// streamLifecycleEvent reports whether a stream event type is pre-output
// lifecycle preamble: the response exists but nothing has been generated yet.
func streamLifecycleEvent(t string) bool {
	switch t {
	case "response.created", "response.in_progress", "response.queued":
		return true
	}
	return false
}

// streamFailureEvent reports whether a stream event type announces a terminal
// failure. Like the lifecycle preamble it carries nothing the model generated,
// so it must not commit an attempt: the whole point of retry/fallback is to
// replace an attempt that ends in one of these. (response.incomplete is NOT
// here: a length-truncated response is output that arrived — committing on it
// is what stops a retry from throwing that output away.)
func streamFailureEvent(t string) bool {
	switch t {
	case "error", "response.error", "response.failed":
		return true
	}
	return false
}

// streamAttempt is the outcome of deliverStreamAttempt — one inner stream
// delivered to a consumer with pre-commit events held back. It is how
// NewRetryModel and NewFallbackModel share one commit rule without duplicating
// the state machine.
type streamAttempt struct {
	// committed: output reached the consumer, so this attempt is the run's
	// final word — emitted tokens cannot be un-sent, and a later error must
	// pass through rather than trigger another attempt.
	committed bool
	// err is the stream's failure; nil means it finished cleanly.
	err error
	// stopped: the consumer returned false mid-delivery; abandon everything,
	// yield nothing further.
	stopped bool
	// pending holds an UNCOMMITTED attempt's events (lifecycle preamble and
	// terminal-failure events), never delivered. The caller flushes it when
	// this attempt turns out to be the last word (clean finish, or a failure
	// no further attempt will follow) and drops it when another attempt
	// supersedes it.
	pending []*TResponseStreamEvent
}

// deliverStreamAttempt consumes one inner stream on behalf of a retrying or
// falling-back decorator. Events that carry nothing the model generated —
// lifecycle preamble, and terminal-failure events — are buffered rather than
// delivered: once delivered they would commit the consumer to a response that
// a retried or swapped attempt then duplicates (a second response.created, a
// different response ID, sequence numbers restarting), and a failure event
// would announce a failure a later attempt goes on to recover from. Holding
// them back until the first output event keeps the consumer's view one
// coherent response no matter how many attempts it took. A nil event neither
// commits nor buffers; it is dropped (the runner tolerates nil the same way).
func deliverStreamAttempt(
	seq iter.Seq2[*TResponseStreamEvent, error],
	yield func(*TResponseStreamEvent, error) bool,
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
func flushStreamEvents(events []*TResponseStreamEvent, yield func(*TResponseStreamEvent, error) bool) bool {
	for _, ev := range events {
		if !yield(ev, nil) {
			return false
		}
	}
	return true
}
