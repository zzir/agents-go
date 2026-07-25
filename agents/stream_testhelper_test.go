package agents

// streamRun drives a RunStream to completion, separating the three things a
// test wants to assert on: the events, the result, and the terminal error.
//
// The old streaming API handed these back through three calls (Events,
// FinalResult, and the per-event error); a stream carries all of them, so this
// is the shape tests use instead.
func streamRun(stream RunStream) (events []StreamEvent, res *RunResult, err error) {
	for ev, eerr := range stream {
		if eerr != nil {
			err = eerr
			break
		}
		if done, ok := ev.(*RunCompletedEvent); ok {
			res = done.Result
			continue
		}
		events = append(events, ev)
	}
	return events, res, err
}
