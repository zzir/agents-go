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
	Data *TResponseStreamEvent
}

func (*RawResponsesStreamEvent) streamEvent() {}

// RunItemStreamEvent is emitted when the runner produces a new RunItem (a
// message, tool call, tool output, handoff or reasoning item).
type RunItemStreamEvent struct {
	// Name is the event name, e.g. "message_output_created", "tool_called",
	// "tool_output", "handoff_requested", "handoff_occured", "reasoning_item_created".
	Name string
	Item RunItem
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

// ToolProgressEvent is a partial result pushed by a running tool.
//
// It is what makes a long tool call watchable: a command producing output for
// two minutes, a patch applying file by file, a nested agent thinking out loud.
// Without it the only honest thing a UI can show is a spinner.
//
// A progress result is NOT the tool's return value and never reaches the model.
// The tool's actual result is the one it returns; treating progress as an
// answer would let a half-finished thought become the conversation.
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

// RunStream is a run in progress. Ranging over it executes the run: events are
// produced as the loop reaches them, and the loop advances only as they are
// consumed.
//
// Running on the consumer's goroutine is deliberate. Abandoning the stream — a
// break, an early return, a failing test — stops the run, instead of leaking
// the goroutine that was producing it. The previous design pushed events into a
// buffered channel from a goroutine and required the consumer to cancel the
// run's context on early exit; that was easy to forget and impossible to
// detect.
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
	// A tool emitting progress runs on its own goroutine while the loop waits
	// on the batch, and several tools emit at once. An iterator's yield is not
	// safe for concurrent calls, so the mutex is what makes ToolContext.Emit
	// possible at all — without it, progress from two parallel tools would
	// corrupt the consumer's range loop.
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
func (r *runner) emitItem(it RunItem) bool {
	if hc, ok := it.(*HandoffCallItem); ok {
		if !r.emit(&RunItemStreamEvent{Name: "tool_called", Item: &ToolCallItem{Agent: hc.Agent, Raw: hc.Raw}}) {
			return false
		}
	}
	return r.emit(&RunItemStreamEvent{Name: runItemEventName(it), Item: it})
}

// runItemEventName maps a RunItem to its stream event name.
func runItemEventName(item RunItem) string {
	switch item.(type) {
	case *MessageOutputItem:
		return "message_output_created"
	case *ToolCallItem:
		return "tool_called"
	case *ToolCallOutputItem:
		return "tool_output"
	case *HandoffCallItem:
		return "handoff_requested"
	case *HandoffOutputItem:
		// (sic) — the misspelling is the wire name consumers already match on.
		return "handoff_occured"
	case *ReasoningItem:
		return "reasoning_item_created"
	default:
		return "unknown"
	}
}
