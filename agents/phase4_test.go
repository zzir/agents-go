package agents

import (
	"context"
	"errors"
	"testing"
)

// --- Guardrails ---

func TestInputGuardrailTripwire(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		InputGuardrails: []InputGuardrail{{
			Name: "block",
			Run: func(_ context.Context, rc *RunContext, agent *Agent, input []TResponseInputItem) (GuardrailFunctionOutput, error) {
				return GuardrailFunctionOutput{TripwireTriggered: true}, nil
			},
		}},
	}
	_, err := Run(context.Background(), agent, "hi", RunOptions{})
	var tw *InputGuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("expected InputGuardrailTripwireError, got %T (%v)", err, err)
	}
	if tw.Result.Guardrail.Name != "block" {
		t.Errorf("guardrail name = %q", tw.Result.Guardrail.Name)
	}
}

func TestOutputGuardrailTripwire(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "leak"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		OutputGuardrails: []OutputGuardrail{{
			Name: "pii",
			Run: func(_ context.Context, rc *RunContext, agent *Agent, output any) (GuardrailFunctionOutput, error) {
				return GuardrailFunctionOutput{TripwireTriggered: output == "leak"}, nil
			},
		}},
	}
	_, err := Run(context.Background(), agent, "hi", RunOptions{})
	var tw *OutputGuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("expected OutputGuardrailTripwireError, got %T (%v)", err, err)
	}
}

func TestToolInputGuardrailReject(t *testing.T) {
	tool := NewFunctionTool("danger", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		t.Error("tool should not run when input guardrail rejects")
		return "ran", nil
	})
	tool.InputGuardrails = []ToolInputGuardrail{{
		Name: "guard",
		Run: func(ctx context.Context, rc *RunContext, d ToolInputGuardrailData) (ToolGuardrailFunctionOutput, error) {
			return RejectToolContent("not allowed", nil), nil
		},
	}}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "danger", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The rejection message should appear as a tool output item.
	var found bool
	for _, it := range res.NewItems {
		if o, ok := it.(*ToolCallOutputItem); ok && o.Output == "not allowed" {
			found = true
		}
	}
	if !found {
		t.Error("expected rejected tool output 'not allowed' in items")
	}
}

func TestToolOutputGuardrailRaise(t *testing.T) {
	tool := NewFunctionTool("leaky", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "secret", nil
	})
	tool.OutputGuardrails = []ToolOutputGuardrail{{
		Name: "guard",
		Run: func(ctx context.Context, rc *RunContext, d ToolOutputGuardrailData) (ToolGuardrailFunctionOutput, error) {
			return RaiseToolException(nil), nil
		},
	}}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "leaky", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	_, err := Run(context.Background(), agent, "go", RunOptions{})
	var tw *ToolGuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("expected ToolGuardrailTripwireError, got %T (%v)", err, err)
	}
}

// --- Session ---

func TestInMemorySession(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "hi there")),
		modelResp(messageOutput(t, "you said hello before")),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	// First run.
	if _, err := Run(ctx, agent, "hello", RunOptions{Session: sess}); err != nil {
		t.Fatal(err)
	}
	items, _ := sess.GetItems(ctx, 0)
	if len(items) < 2 {
		t.Fatalf("session should have user input + assistant msg, got %d", len(items))
	}

	// Second run: history must be prepended to the model input.
	if _, err := Run(ctx, agent, "what did I say?", RunOptions{Session: sess}); err != nil {
		t.Fatal(err)
	}
	// The second call's input should include the prior turn's items plus both
	// user messages.
	// 2 prior items (user "hello" + assistant) + new user message = 3.
	if len(model.lastReq.Input) < 3 {
		t.Errorf("second run input too short (history not prepended): %d", len(model.lastReq.Input))
	}
}

// --- Streaming ---

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
	if agentUpdated != 1 {
		t.Errorf("agent_updated events = %d, want 1", agentUpdated)
	}
	res, _ := sr.FinalResult()
	if res.LastAgent != billing {
		t.Errorf("last agent = %v, want billing", res.LastAgent.Name)
	}
}
