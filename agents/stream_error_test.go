package agents

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
)

// cancelStreamModel cancels the run's context as its stream starts, then yields a
// context.Canceled error — reproducing a provider surfacing a mid-stream
// cancellation (e.g. "openai responses stream: context canceled").
type cancelStreamModel struct{ cancel context.CancelFunc }

func (m *cancelStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, context.Canceled
}

func (m *cancelStreamModel) StreamResponse(ctx context.Context, _ ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		m.cancel()
		yield(nil, context.Canceled)
	}
}

// A streamed run cancelled mid-stream must report its terminal error via
// FinalResult even when the Events channel drops it (RunStreamed's error send
// loses the select to ctx.Done()). agents-server relies on this: it consults
// FinalResult after draining Events so an aborted run never vanishes.
func TestRunStreamed_TerminalErrorAlwaysInFinalResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	agent := &Agent{Name: "a", ModelImpl: &cancelStreamModel{cancel: cancel}}

	sr := RunStreamed(ctx, agent, "hi", RunOptions{})

	// Drain the events WITHOUT inspecting the per-item error, mimicking the drop:
	// the failure may or may not be delivered here.
	for range sr.Events() { //nolint:revive // intentional drain
	}

	_, err := sr.FinalResult()
	if err == nil {
		t.Fatal("FinalResult must surface the terminal error even when Events drops it")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("FinalResult err = %v, want context.Canceled", err)
	}
}

// StopAfterTurn finishes the in-flight turn (including its tool) and stops
// cleanly before the next turn, with no error and no final output.
func TestStreamedResult_StopAfterTurn(t *testing.T) {
	released := make(chan struct{})
	var once sync.Once
	gate := NewFunctionTool("gate", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		<-released
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "gate", "c1", `{}`)),
		modelResp(functionCallOutput(t, "gate", "c2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{gate}, ModelImpl: model}

	sr := RunStreamed(context.Background(), agent, "go", RunOptions{})
	for ev, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
		if ie, ok := ev.(*RunItemStreamEvent); ok && ie.Name == "tool_called" {
			// The turn-1 tool is emitted but still blocked; ask for a graceful
			// stop, then let the tool finish so the turn completes.
			sr.StopAfterTurn()
			once.Do(func() { close(released) })
		}
	}
	res, err := sr.FinalResult()
	if err != nil {
		t.Fatalf("graceful stop returned error: %v", err)
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 (stopped after turn 1)", model.calls)
	}
	if res.FinalOutput != nil {
		t.Errorf("final output = %v, want nil after graceful stop", res.FinalOutput)
	}
	if len(res.Interruptions) != 0 {
		t.Errorf("interruptions = %d, want 0", len(res.Interruptions))
	}
}

// A fresh streamed run emits an AgentUpdatedStreamEvent for the starting
// agent before any turn runs.
func TestStreamedResult_InitialAgentUpdated(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hi"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	sr := RunStreamed(context.Background(), agent, "go", RunOptions{})
	var agentUpdated int
	first := true
	for ev, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
		if au, ok := ev.(*AgentUpdatedStreamEvent); ok {
			agentUpdated++
			if first && au.NewAgent != agent {
				t.Errorf("first agent_updated agent = %v, want a", au.NewAgent)
			}
		}
		first = false
	}
	if agentUpdated != 1 {
		t.Errorf("agent_updated events = %d, want 1", agentUpdated)
	}
}

// A handoff surfaces as BOTH a tool_called and a handoff_requested event.
func TestStreamedResult_HandoffEmitsToolCalled(t *testing.T) {
	billing := &Agent{Name: "billing", ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "handled"))}}}
	triage := &Agent{
		Name:      "triage",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_billing", "h1", `{}`))}},
		Handoffs:  []Handoff{HandoffTo(billing)},
	}
	sr := RunStreamed(context.Background(), triage, "q", RunOptions{})
	var toolCalledHandoff, handoffRequested int
	for ev, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
		ie, ok := ev.(*RunItemStreamEvent)
		if !ok {
			continue
		}
		switch ie.Name {
		case "tool_called":
			if tc, ok := ie.Item.(*ToolCallItem); ok && tc.FunctionCall().CallID == "h1" {
				toolCalledHandoff++
			}
		case "handoff_requested":
			handoffRequested++
		}
	}
	if toolCalledHandoff != 1 {
		t.Errorf("handoff tool_called events = %d, want 1", toolCalledHandoff)
	}
	if handoffRequested != 1 {
		t.Errorf("handoff_requested events = %d, want 1", handoffRequested)
	}
}

// A streaming run that fails before its first model call (here via an
// input-guardrail tripwire) must leave no orphan user message in the session.
func TestStreamedResult_NoOrphanInputOnPreModelFailure(t *testing.T) {
	session := NewInMemorySession()
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "unused"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "block",
			Stages: []GuardrailStage{StageInput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Trip(nil), nil
			},
		}},
	}

	sr := RunStreamed(context.Background(), agent, "hi", RunOptions{Session: session})
	var streamErr error
	for _, err := range sr.Events() {
		if err != nil {
			streamErr = err
		}
	}
	_, ferr := sr.FinalResult()
	var tw *GuardrailTripwireError
	if !errors.As(ferr, &tw) && !errors.As(streamErr, &tw) {
		t.Fatalf("expected *GuardrailTripwireError, got final=%v stream=%v", ferr, streamErr)
	}
	// The guardrail tripped before the model call, so nothing was persisted.
	items, _ := session.GetItems(context.Background(), 0)
	if len(items) != 0 {
		t.Errorf("session has %d orphan items, want 0", len(items))
	}
	if model.calls != 0 {
		t.Errorf("model was called %d times, want 0", model.calls)
	}
}

func TestRunStreamed_Events(t *testing.T) {
	tool := NewFunctionTool("get_weather", "", func(ctx context.Context, tc *ToolContext, a struct {
		City string `json:"city"`
	}) (string, error) {
		return "sunny", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "c1", `{"city":"SF"}`)),
		modelResp(messageOutput(t, "it is sunny")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	sr := RunStreamed(context.Background(), agent, "weather?", RunOptions{})
	var raw, toolCalled, toolOutput, message int
	for event, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
		switch e := event.(type) {
		case *RawResponsesStreamEvent:
			raw++
		case *RunItemStreamEvent:
			switch e.Name {
			case "tool_called":
				toolCalled++
			case "tool_output":
				toolOutput++
			case "message_output_created":
				message++
			}
		}
	}
	res, err := sr.FinalResult()
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "it is sunny" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if raw < 2 {
		t.Errorf("expected >=2 raw events (one per turn), got %d", raw)
	}
	if toolCalled != 1 {
		t.Errorf("tool_called events = %d, want 1", toolCalled)
	}
	if toolOutput != 1 {
		t.Errorf("tool_output events = %d, want 1", toolOutput)
	}
	if message != 1 {
		t.Errorf("message events = %d, want 1", message)
	}
}

func TestRunStreamed_AgentUpdatedOnHandoff(t *testing.T) {
	billing := &Agent{Name: "billing"}
	billing.ModelImpl = &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "handled"))}}
	triage := &Agent{
		Name:      "triage",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_billing", "c1", `{}`))}},
		Handoffs:  []Handoff{HandoffTo(billing)},
	}
	sr := RunStreamed(context.Background(), triage, "billing q", RunOptions{})
	var agentUpdated int
	for event, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := event.(*AgentUpdatedStreamEvent); ok {
			agentUpdated++
		}
	}
	// Two agent_updated events: one for the starting agent before the first turn,
	// and one when control transfers to billing.
	if agentUpdated != 2 {
		t.Errorf("agent_updated events = %d, want 2", agentUpdated)
	}
	res, _ := sr.FinalResult()
	if res.LastAgent != billing {
		t.Errorf("last agent = %v, want billing", res.LastAgent.Name)
	}
}
