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

// A failing run always reports its error through the stream. The old design
// pushed events into a channel and kept the error on the side, so a consumer
// that stopped early could lose it and had to consult FinalResult separately;
// there is one path now, and it cannot drop the error.
func TestRunStream_TerminalErrorAlwaysReachesTheConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	agent := &Agent{Name: "a", ModelImpl: &cancelStreamModel{cancel: cancel}}

	stream, _ := Run(ctx, agent, "hi", RunOptions{})
	_, res, err := streamRun(stream)

	if err == nil {
		t.Fatal("the stream must surface the terminal error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res != nil {
		t.Errorf("a failed run must not also report a result: %+v", res)
	}

	// Collect reports the same error rather than the "no result" placeholder.
	stream2, _ := Run(ctx, agent, "hi", RunOptions{})
	if _, cerr := stream2.Collect(); !errors.Is(cerr, context.Canceled) {
		t.Errorf("Collect err = %v, want context.Canceled", cerr)
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

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{})
	var res *RunResult
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		if done, ok := ev.(*RunCompletedEvent); ok {
			res = done.Result
		}
		if ie, ok := ev.(*RunItemStreamEvent); ok && ie.Name == "tool_called" {
			// The turn-1 tool is emitted but still blocked; ask for a graceful
			// stop, then let the tool finish so the turn completes.
			ctrl.StopAfterTurn()
			once.Do(func() { close(released) })
		}
	}
	if res == nil {
		t.Fatal("graceful stop produced no result")
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

	stream, _ := Run(context.Background(), agent, "go", RunOptions{})
	var agentUpdated int
	first := true
	for ev, err := range stream {
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
	stream, _ := Run(context.Background(), triage, "q", RunOptions{})
	var toolCalledHandoff, handoffRequested int
	for ev, err := range stream {
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

// Whether a tripped input guardrail leaves an orphan user message — and
// whether it spends tokens — is decided by Blocking, and by nothing else.
//
// The two entry points used to disagree here: streaming ran input guardrails
// synchronously before the first model call, blocking raced them. One loop
// means one answer, and Blocking is the knob that was already documented for
// exactly this ("use it when a tripwire must prevent any token spend").
func TestInputGuardrailTripwire_PersistenceDependsOnBlocking(t *testing.T) {
	newAgent := func(model *fakeModel, blocking bool) *Agent {
		return &Agent{
			Name:      "a",
			ModelImpl: model,
			Guardrails: []Guardrail{{
				Name:     "block",
				Stages:   []GuardrailStage{StageInput},
				Blocking: blocking,
				Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
					return Trip(nil), nil
				},
			}},
		}
	}
	tripped := func(t *testing.T, err error) {
		t.Helper()
		var tw *GuardrailTripwireError
		if !errors.As(err, &tw) {
			t.Fatalf("expected *GuardrailTripwireError, got %v", err)
		}
	}

	// Blocking: the guardrail is a gate. It finishes before anything is
	// persisted and before the model is reached, so a tripwire leaves the
	// session untouched and costs nothing.
	t.Run("blocking leaves nothing behind", func(t *testing.T) {
		session := NewInMemorySession()
		model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "unused"))}}
		stream, _ := Run(context.Background(), newAgent(model, true), "hi", RunOptions{Conversation: ConversationOptions{Session: session}})
		_, _, err := streamRun(stream)
		tripped(t, err)

		if items, _ := session.GetItems(context.Background(), 0); len(items) != 0 {
			t.Errorf("session has %d orphan items, want 0", len(items))
		}
		if model.calls != 0 {
			t.Errorf("model was called %d times, want 0", model.calls)
		}
	})

	// Racing (the default): the guardrail runs alongside the model call, so by
	// the time it trips the input is persisted and the request is in flight.
	// That is the trade for not serializing every guardrail ahead of every
	// model call — and why Blocking exists.
	t.Run("racing persists the input", func(t *testing.T) {
		session := NewInMemorySession()
		model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "unused"))}}
		stream, _ := Run(context.Background(), newAgent(model, false), "hi", RunOptions{Conversation: ConversationOptions{Session: session}})
		_, _, err := streamRun(stream)
		tripped(t, err)

		items, _ := session.GetItems(context.Background(), 0)
		if len(items) != 1 {
			t.Errorf("session has %d items, want the 1 user input", len(items))
		}
	})

	// RunSync must answer the same way — the two entry points share the loop.
	t.Run("RunSync agrees", func(t *testing.T) {
		session := NewInMemorySession()
		model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "unused"))}}
		_, err := RunSync(context.Background(), newAgent(model, true), "hi", RunOptions{Conversation: ConversationOptions{Session: session}})
		tripped(t, err)

		if items, _ := session.GetItems(context.Background(), 0); len(items) != 0 {
			t.Errorf("RunSync left %d orphan items where Run leaves 0", len(items))
		}
		if model.calls != 0 {
			t.Errorf("RunSync called the model %d times where Run calls 0", model.calls)
		}
	})
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

	stream, _ := Run(context.Background(), agent, "weather?", RunOptions{})
	var raw, toolCalled, toolOutput, message int
	events, res, err := streamRun(stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
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
	stream, _ := Run(context.Background(), triage, "billing q", RunOptions{})
	var agentUpdated int
	events, res, err := streamRun(stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if _, ok := event.(*AgentUpdatedStreamEvent); ok {
			agentUpdated++
		}
	}
	// Two agent_updated events: one for the starting agent before the first turn,
	// and one when control transfers to billing.
	if agentUpdated != 2 {
		t.Errorf("agent_updated events = %d, want 2", agentUpdated)
	}
	if res.LastAgent != billing {
		t.Errorf("last agent = %v, want billing", res.LastAgent.Name)
	}
}
