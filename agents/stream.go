package agents

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
)

// StreamEvent is the sealed interface for events emitted during a streamed run.
// Type-switch on the concrete types: *RawResponsesStreamEvent,
// *RunItemStreamEvent, *AgentUpdatedStreamEvent.
type StreamEvent interface{ streamEvent() }

// RawResponsesStreamEvent wraps a raw Responses API streaming event, passed
// through directly from the model.
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

// StreamedResult is the handle returned by RunStreamed. Consume events with
// Events; once iteration completes, FinalResult returns the completed run.
//
// If the consumer stops iterating early, it must cancel the run's context —
// otherwise the producing goroutine stays blocked on the event channel.
type StreamedResult struct {
	ch chan streamMsg

	mu           sync.Mutex
	final        *RunResult
	err          error
	currentAgent *Agent
	currentTurn  int

	// stopAfterTurn, when set via StopAfterTurn, asks the run loop to stop after
	// the current turn finishes (tools and session save included). Atomic because
	// the consumer sets it while the run loop reads it concurrently.
	stopAfterTurn atomic.Bool
}

// StopAfterTurn requests a graceful stop of a streaming run: the in-flight turn
// finishes — including its tool calls and session save — and then the run stops
// cleanly, with no error and a nil FinalOutput, before the next turn begins. To
// stop immediately instead, cancel the run's context. It is the Go counterpart
// of Python's StreamedResult.cancel(mode="after_turn"); the "immediate" mode is
// covered by context cancellation.
func (s *StreamedResult) StopAfterTurn() { s.stopAfterTurn.Store(true) }

// stopRequested reports whether StopAfterTurn was called.
func (s *StreamedResult) stopRequested() bool { return s.stopAfterTurn.Load() }

// CurrentAgent returns the agent handling the turn currently in progress, or nil
// before the first turn starts. Safe to call from the event-consuming goroutine.
func (s *StreamedResult) CurrentAgent() *Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentAgent
}

// CurrentTurn returns the 1-based number of the turn currently in progress, or 0
// before the first turn starts. Safe to call from the event-consuming goroutine.
func (s *StreamedResult) CurrentTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentTurn
}

// setCurrent records the live agent and turn number for CurrentAgent/CurrentTurn.
func (s *StreamedResult) setCurrent(agent *Agent, turn int) {
	s.mu.Lock()
	s.currentAgent = agent
	s.currentTurn = turn
	s.mu.Unlock()
}

type streamMsg struct {
	event StreamEvent
	err   error
}

// Events returns an iterator over the run's stream events. The second value is
// a terminal error; when it is non-nil, iteration stops. Iterate to completion
// (or break early) before calling FinalResult.
func (s *StreamedResult) Events() iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		for msg := range s.ch {
			if msg.err != nil {
				yield(nil, msg.err)
				return
			}
			if !yield(msg.event, nil) {
				return
			}
		}
	}
}

// FinalResult returns the completed run result and any terminal error. Call it
// after the Events iterator is exhausted.
func (s *StreamedResult) FinalResult() (*RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final, s.err
}

func (s *StreamedResult) setFinal(res *RunResult, err error) {
	s.mu.Lock()
	s.final = res
	s.err = err
	s.mu.Unlock()
}

// RunStreamed executes the agent loop like Run, but streams events as they are
// produced. The returned StreamedResult's Events iterator yields raw model
// events, run-item events and agent-update events; FinalResult returns the
// completed run once the iterator is exhausted.
func RunStreamed(ctx context.Context, agent *Agent, input any, opts RunOptions) *StreamedResult {
	sr := &StreamedResult{ch: make(chan streamMsg, 64)}

	go func() {
		defer close(sr.ch)
		res, err := runStreamedLoop(ctx, agent, input, opts, sr)
		sr.setFinal(res, err)
		if err != nil {
			// The error is already recorded via setFinal, so when the consumer
			// has gone away (context canceled) and the buffer is full, dropping
			// this send is safe. A bare send here would block forever and leak
			// this goroutine, keeping the channel from ever closing.
			select {
			case sr.ch <- streamMsg{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	return sr
}

// emit sends an event to the consumer; the send is dropped when the context is
// done (the run loop notices the cancellation at its next turn check).
func (s *StreamedResult) emit(ctx context.Context, event StreamEvent) {
	select {
	case s.ch <- streamMsg{event: event}:
	case <-ctx.Done():
	}
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
