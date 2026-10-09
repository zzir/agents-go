package agents

import (
	"errors"
	"iter"
)

// StreamEvent is the sealed interface for events emitted during a run;
// type-switch on the concrete types.
type StreamEvent interface{ streamEvent() }

// RawResponsesStreamEvent wraps a raw Responses API streaming event from the
// model. Only a run started with Run emits them.
type RawResponsesStreamEvent struct {
	Data *ResponseStreamEvent
}

func (*RawResponsesStreamEvent) streamEvent() {}

// RunItemStreamEvent is emitted when the runner produces a new RunItem. A
// handoff call arrives twice: as an ItemToolCall with IsHandoff set, then as
// an ItemHandoffCall — see spec §2.4.
type RunItemStreamEvent struct {
	Item *RunItem
}

func (*RunItemStreamEvent) streamEvent() {}

// AgentUpdatedStreamEvent is emitted when control passes to a new agent.
type AgentUpdatedStreamEvent struct {
	NewAgent *Agent
}

func (*AgentUpdatedStreamEvent) streamEvent() {}

// RunCompletedEvent is a run's terminal event, carrying the finished result:
// exactly once, last, on a run that ends without error; a failing run emits none.
type RunCompletedEvent struct {
	Result *RunResult
}

func (*RunCompletedEvent) streamEvent() {}

// ToolProgressEvent is a partial result pushed by a running tool; it never
// reaches the model — see spec §2.7g.
type ToolProgressEvent struct {
	// ToolName and CallID identify the call; key on CallID, several tools
	// stream at once.
	ToolName string
	CallID   string
	// Agent is the agent whose tool is running.
	Agent *Agent
	// Result is the partial result: Content to show, Details and Display for
	// the renderer.
	Result ToolResult
}

func (*ToolProgressEvent) streamEvent() {}

// ItemsPersistedEvent reports that every run item streamed before it is now in
// the session; its absence promises nothing — see spec §2.5.
type ItemsPersistedEvent struct{}

func (*ItemsPersistedEvent) streamEvent() {}

// RunStream is a run in progress. Ranging over it executes the run on the
// consumer's goroutine and abandoning it stops the run — see spec §2.0. A
// non-nil error is terminal; Fanout feeds several consumers.
type RunStream iter.Seq2[StreamEvent, error]

// Collect drives the stream to completion and returns the run's result; a
// stream that ended without one (stopped early) is an error.
func (s RunStream) Collect() (*RunResult, error) {
	var res *RunResult
	for ev, err := range s {
		if err != nil {
			return nil, err
		}
		if done, ok := ev.(*RunCompletedEvent); ok {
			res = done.Result
		}
	}
	if res == nil {
		return nil, errors.New("agents: the run stream ended without a result; it was stopped before completing")
	}
	return res, nil
}

// errConsumerStopped unwinds the run loop when the consumer stops ranging; it
// never reaches the caller.
var errConsumerStopped = errors.New("agents: stream consumer stopped")

// emit yields an event and reports whether the run should continue; a false
// return propagates to the loop as errConsumerStopped.
func (r *runner) emit(event StreamEvent) bool {
	// Tool progress arrives from other goroutines; yield is not
	// concurrency-safe — see spec §2.7g.
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.closed.Load() {
		return false
	}
	if !r.yield(event, nil) {
		r.closed.Store(true)
		// In-flight work (a streamed model call, a tool batch) has no emit
		// point to observe the flag at.
		if r.cancelRun != nil {
			r.cancelRun(errConsumerStopped)
		}
		return false
	}
	return true
}

// emitItem emits a run item's stream event; a handoff call is preceded by the
// tool-call event wrapping it.
func (r *runner) emitItem(it *RunItem) bool {
	if it.Kind == ItemHandoffCall {
		wrapped := &RunItem{Kind: ItemToolCall, Agent: it.Agent, Raw: it.Raw, IsHandoff: true}
		if !r.emit(&RunItemStreamEvent{Item: wrapped}) {
			return false
		}
	}
	return r.emit(&RunItemStreamEvent{Item: it})
}
