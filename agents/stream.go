package agents

import (
	"errors"
	"iter"
	"sync"
	"sync/atomic"
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

// RunPhase says what a run is doing right now. It is advisory — useful for a
// progress indicator during a long silence — and a run moves between phases
// many times per turn.
type RunPhase int32

const (
	// PhaseIdle is before the first turn and after the last.
	PhaseIdle RunPhase = iota
	// PhaseGuardrails is running input or output guardrails.
	PhaseGuardrails
	// PhaseModelCall is waiting on the model.
	PhaseModelCall
	// PhaseToolExecution is running tools.
	PhaseToolExecution
	// PhasePersisting is writing the turn to the session.
	PhasePersisting
	// PhaseCompaction is compacting session history.
	PhaseCompaction
)

func (p RunPhase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhaseGuardrails:
		return "guardrails"
	case PhaseModelCall:
		return "model"
	case PhaseToolExecution:
		return "tools"
	case PhasePersisting:
		return "persisting"
	case PhaseCompaction:
		return "compaction"
	}
	return "unknown"
}

// RunControl influences a run while it is in flight. Run returns one alongside
// the stream; it is safe to use from another goroutine, including before
// ranging has begun.
type RunControl interface {
	// StopAfterTurn asks the run to stop once the in-flight turn finishes —
	// tools and session save included — ending cleanly with no error before the
	// next turn begins.
	//
	// This is not the same as abandoning the stream. Breaking out of the range
	// loop stops the run where it stands, mid-turn, with that turn's work
	// unfinished; cancelling the context does the same, harder. StopAfterTurn
	// is the one that leaves the session consistent.
	StopAfterTurn()

	// Phase reports what the run is doing right now.
	Phase() RunPhase

	// CurrentAgent returns the agent handling the turn in progress, or nil
	// before the first turn starts.
	CurrentAgent() *Agent

	// CurrentTurn returns the 1-based number of the turn in progress, or 0
	// before the first turn starts.
	CurrentTurn() int
}

// runControl is the concrete RunControl. It is created before the run starts —
// the caller holds it from the moment Run returns and may read it before
// ranging — so every field is written by the run loop and read concurrently.
type runControl struct {
	stopAfterTurn atomic.Bool
	phase         atomic.Int32

	mu           sync.Mutex
	currentAgent *Agent
	currentTurn  int
}

func newRunControl() *runControl { return &runControl{} }

func (c *runControl) StopAfterTurn() { c.stopAfterTurn.Store(true) }

func (c *runControl) stopRequested() bool { return c != nil && c.stopAfterTurn.Load() }

func (c *runControl) Phase() RunPhase { return RunPhase(c.phase.Load()) }

func (c *runControl) setPhase(p RunPhase) {
	if c != nil {
		c.phase.Store(int32(p))
	}
}

func (c *runControl) CurrentAgent() *Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentAgent
}

func (c *runControl) CurrentTurn() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentTurn
}

func (c *runControl) setCurrent(agent *Agent, turn int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.currentAgent = agent
	c.currentTurn = turn
	c.mu.Unlock()
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
	if !r.yield(event, nil) {
		r.consumerStopped = true
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
		return "handoff_occured" // (sic) matches the Python SDK's spelling
	case *ReasoningItem:
		return "reasoning_item_created"
	default:
		return "unknown"
	}
}
