// Package middleware holds run middleware: optional policy layered over the run
// loop rather than built into it.
//
// A middleware wraps a whole run. That is what it is good at — retrying it,
// running it again with feedback, resuming it from an interruption — and also
// what bounds it. Anything that has to reach inside a turn stays in the loop:
//
//   - ExecOptions.ErrorHandlers recovers a failing run by supplying a fallback
//     final output, and needs the loop's in-flight items to build RunErrorData
//     and the loop's completion path to persist the result. A middleware sees
//     a terminal error and cannot reconstruct either.
//   - Guardrails race the model call and can cancel it; handoffs move the state
//     machine; session persistence has a boundary only the loop knows.
//
// Expressing those as middleware would turn invariants into implicit protocols
// between wrappers.
package middleware

import (
	"github.com/zzir/agents-go/agents"
)

// collect drives a stream to its result, forwarding every event to yield so a
// middleware that re-enters the run still streams the attempts it discards.
//
// A middleware that swallowed events until it was satisfied would make a long
// retry look like a hang, which is the opposite of what streaming is for.
//
// live reports whether the consumer is still reading. When it is false the
// caller must return immediately without yielding anything more — including an
// error, which nobody is there to receive.
func collect(s agents.RunStream, yield func(agents.StreamEvent, error) bool) (res *agents.RunResult, live bool, err error) {
	for ev, cerr := range s {
		if cerr != nil {
			return nil, true, cerr
		}
		if done, ok := ev.(*agents.RunCompletedEvent); ok {
			// Held back: the middleware decides whether this is the run's
			// completion or one attempt within it.
			res = done.Result
			continue
		}
		if !yield(ev, nil) {
			return nil, false, nil
		}
	}
	return res, true, nil
}

// finish emits a result as the run's terminal event.
func finish(res *agents.RunResult, yield func(agents.StreamEvent, error) bool) {
	yield(&agents.RunCompletedEvent{Result: res}, nil)
}
