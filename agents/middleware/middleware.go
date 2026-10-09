// Package middleware holds run middleware: optional policy that wraps a whole
// run; the stream contract an author owes is in spec §2.12.
package middleware

import (
	"github.com/zzir/agents-go/agents"
)

// collect drives a stream to its result, forwarding all but RunCompletedEvent
// (spec §2.12). live=false means the consumer left: yield nothing more, not
// even an error.
func collect(s agents.RunStream, yield func(agents.StreamEvent, error) bool) (res *agents.RunResult, live bool, err error) {
	for ev, cerr := range s {
		if cerr != nil {
			return nil, true, cerr
		}
		if done, ok := ev.(*agents.RunCompletedEvent); ok {
			// Held back: the middleware decides whether this completes the run
			// or one attempt.
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
