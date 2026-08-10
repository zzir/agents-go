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

// RunCompletedEvent is a run's terminal event, carrying the finished result. It
// is emitted exactly once, last, on a run that ends without error; a run that
// fails ends with a non-nil error and emits no completion.
//
// It is how a stream carries its result, and why there is no separate
// FinalResult call to forget. Collect exists so consumers rarely match on it by
// hand.
type RunCompletedEvent struct {
	Result *RunResult
}

func (*RunCompletedEvent) streamEvent() {}

// ToolProgressEvent is a partial result pushed by a running tool. It is what
// makes a long tool call watchable rather than a spinner.
//
// A progress result is NOT the tool's return value and never reaches the model:
// the tool's actual result is the one it returns.
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

// ItemsPersistedEvent reports that every run item that appeared on the stream
// before it has been written to the session. It fires at each persist boundary
// that leaves nothing behind — the per-turn save, a handoff, overflow
// recovery, the final save — so a consumer buffering streamed content against
// a crash can drop what this event just guaranteed, instead of inferring the
// SDK's persist timing from raw response events.
//
// The implication is one-way: no event does not mean nothing persisted. A run
// without a session never emits it; history restored on resume was persisted
// before the stream began; and a save that held items back (an interruption's
// pending calls persist only on resume) is not announced, precisely because
// the stream has shown items the store does not yet hold.
type ItemsPersistedEvent struct{}

func (*ItemsPersistedEvent) streamEvent() {}

// RunStream is a run in progress. Ranging over it executes the run: events are
// produced as the loop reaches them, and the loop advances only as they are
// consumed.
//
// Running on the consumer's goroutine is deliberate. Abandoning the stream — a
// break, an early return, a failing test — stops the run, instead of leaking the
// goroutine that was producing it.
//
// The cost is that a slow consumer slows the run. For a single consumer that is
// backpressure working correctly. For one run feeding several consumers at
// different speeds — a server broadcasting to several browsers — put a Fanout
// between them: it buffers per subscriber and reports whatever it had to drop.
//
// The second value is a terminal error; when it is non-nil the stream ends.
type RunStream iter.Seq2[StreamEvent, error]

// Collect drives the stream to completion and returns the run's result,
// discarding intermediate events. It turns a stream back into a plain call.
//
// It reports an error if the stream ended without a result, which happens only
// when something stopped it early.
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

// errConsumerStopped unwinds the run loop when the consumer stops ranging the
// stream. It never reaches the caller: yield has already returned false, so
// there is nobody to report it to — the loop only needs to stop and let its
// defers run.
var errConsumerStopped = errors.New("agents: stream consumer stopped")

// emit yields an event, reporting whether the run should continue. Every emit
// site must propagate a false return; the loop turns it into errConsumerStopped
// so the unwind path is the same as any other abort.
func (r *runner) emit(event StreamEvent) bool {
	if r.yield == nil {
		return true
	}
	// A tool emitting progress runs on its own goroutine, and several emit at
	// once. An iterator's yield is not safe for concurrent calls, so this mutex
	// is what makes ToolContext.Emit possible at all.
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.consumerStopped.Load() {
		// Another goroutine already saw the consumer leave; calling yield again
		// after it returned false is undefined.
		return false
	}
	if !r.yield(event, nil) {
		r.consumerStopped.Store(true)
		// Tell everything riding the run's context — the model call the loop
		// is streaming, the tool batch it is waiting on — to stop now: the
		// loop only observes the flag at emit points, and a batch in
		// g.Wait() has none.
		if r.cancelRun != nil {
			r.cancelRun(errConsumerStopped)
		}
		return false
	}
	return true
}

// emitItem emits a run item's stream event. A handoff call additionally emits a
// tool_called event wrapping the underlying function call, so a handoff
// surfaces as BOTH tool_called and handoff_requested — matching the model's own
// view, where the handoff is a tool call.
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
		// "unknown" belongs to ItemUnknown alone — a wire type this build does
		// not model. A kind the SDK does model must never borrow it: a consumer
		// matching on the name could not tell the two apart.
		return "unknown"
	}
}
