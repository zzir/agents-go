package agents

import (
	"errors"
	"iter"
)

// StreamEvent is the sealed interface for events emitted during a run.
// Type-switch on the concrete types: *RawResponsesStreamEvent,
// *RunItemStreamEvent, *AgentUpdatedStreamEvent, *RunCompletedEvent.
type StreamEvent interface{ streamEvent() }

// RawResponsesStreamEvent wraps a raw Responses API streaming event, passed
// through directly from the model. It appears only on runs started with Run;
// RunSync calls the model without streaming and produces none.
type RawResponsesStreamEvent struct {
	Data *ResponseStreamEvent
}

func (*RawResponsesStreamEvent) streamEvent() {}

// RunItemStreamEvent is emitted when the runner produces a new RunItem (a
// message, tool call, tool output, handoff or reasoning item).
type RunItemStreamEvent struct {
	// Name is the event name, e.g. "message_output_created", "tool_called",
	// "tool_output", "handoff_requested", "handoff_occured",
	// "reasoning_item_created", "injected_input_created".
	Name string
	Item *RunItem
}

func (*RunItemStreamEvent) streamEvent() {}

// AgentUpdatedStreamEvent is emitted when control passes to a new agent.
type AgentUpdatedStreamEvent struct {
	NewAgent *Agent
}

func (*AgentUpdatedStreamEvent) streamEvent() {}

// RunCompletedEvent is a run's terminal event, carrying the finished result:
// emitted exactly once, last, on a run that ends without error; a failing run
// emits none. Collect matches on it so consumers rarely do by hand.
type RunCompletedEvent struct {
	Result *RunResult
}

func (*RunCompletedEvent) streamEvent() {}

// ToolProgressEvent is a partial result pushed by a running tool, which makes
// a long tool call watchable. It is NOT the tool's return value and never
// reaches the model (spec §2.7g).
type ToolProgressEvent struct {
	// ToolName and CallID identify the call this belongs to. The call id is
	// what a consumer keys on: several tools stream at once.
	ToolName string
	CallID   string
	// Agent is the agent whose tool is running.
	Agent *Agent
	// Result is the partial result. Its Content is what to show; Details and
	// Display carry whatever the tool wants a renderer to know.
	Result ToolResult
}

func (*ToolProgressEvent) streamEvent() {}

// ItemsPersistedEvent reports that every run item shown on the stream before
// it is now in the session. It fires at each persist boundary that leaves
// nothing behind; its absence promises nothing — a run without a session
// never emits it, and a save that held items back stays silent (spec §2.5).
type ItemsPersistedEvent struct{}

func (*ItemsPersistedEvent) streamEvent() {}

// RunStream is a run in progress. Ranging over it executes the run on the
// consumer's goroutine: events are produced as the loop reaches them, and
// abandoning the stream stops the run (spec §2.0). A slow consumer slows the
// run; to feed several consumers at different speeds, put a Fanout between
// them. The second value is a terminal error; when non-nil the stream ends.
type RunStream iter.Seq2[StreamEvent, error]

// Collect drives the stream to completion and returns the run's result,
// discarding intermediate events. It reports an error if the stream ended
// without a result, which happens only when something stopped it early.
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

// errConsumerStopped unwinds the run loop when the consumer stops ranging. It
// never reaches the caller: yield already returned false.
var errConsumerStopped = errors.New("agents: stream consumer stopped")

// emit yields an event, reporting whether the run should continue; every emit
// site propagates a false return, which the loop turns into errConsumerStopped.
func (r *runner) emit(event StreamEvent) bool {
	// Tool progress arrives from other goroutines, and an iterator's yield is
	// not safe for concurrent calls: the mutex is what makes Emit possible.
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.closed.Load() {
		// The consumer left, or the terminal event went out: a yield after
		// either has nowhere to go.
		return false
	}
	if !r.yield(event, nil) {
		r.closed.Store(true)
		// Cancel the in-flight work — a streamed model call, a tool batch in
		// g.Wait() — which has no emit point of its own to observe the flag at.
		if r.cancelRun != nil {
			r.cancelRun(errConsumerStopped)
		}
		return false
	}
	return true
}

// emitItem emits a run item's stream event. A handoff call additionally emits
// a tool_called event wrapping the call — the model's own view of a handoff.
func (r *runner) emitItem(it *RunItem) bool {
	if it.Kind == ItemHandoffCall {
		wrapped := &RunItem{Kind: ItemToolCall, Agent: it.Agent, Raw: it.Raw, IsHandoff: true}
		if !r.emit(&RunItemStreamEvent{Name: "tool_called", Item: wrapped}) {
			return false
		}
	}
	return r.emit(&RunItemStreamEvent{Name: runItemEventName(it), Item: it})
}

// runItemEventName maps an item kind to its stream event name.
func runItemEventName(item *RunItem) string {
	switch item.Kind {
	case ItemMessage:
		return "message_output_created"
	case ItemToolCall:
		return "tool_called"
	case ItemToolCallOutput:
		return "tool_output"
	case ItemHandoffCall:
		return "handoff_requested"
	case ItemHandoffOutput:
		// (sic) — the misspelling is the wire name consumers already match on.
		return "handoff_occured"
	case ItemReasoning:
		return "reasoning_item_created"
	case ItemInjectedInput:
		return "injected_input_created"
	default:
		// "unknown" belongs to ItemUnknown alone; a kind the SDK models must
		// never borrow it, or a consumer matching on the name cannot tell them apart.
		return "unknown"
	}
}
