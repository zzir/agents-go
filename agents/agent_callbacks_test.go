package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Per-agent callbacks are what survived the removal of the hook interfaces:
// they are attached to the AGENT, so a handoff swaps them, which run-level
// middleware cannot express.
func TestAgentCallbacks_SwapWithTheAgent(t *testing.T) {
	var order []string
	billing := &Agent{
		Name: "billing",
		OnStart: func(context.Context, *RunContext) error {
			order = append(order, "billing:start")
			return nil
		},
		OnEnd: func(context.Context, *RunContext, any) error {
			order = append(order, "billing:end")
			return nil
		},
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "handled"))}},
	}
	triage := &Agent{
		Name:     "triage",
		Handoffs: []Handoff{HandoffTo(billing)},
		OnStart: func(context.Context, *RunContext) error {
			order = append(order, "triage:start")
			return nil
		},
		OnEnd: func(context.Context, *RunContext, any) error {
			order = append(order, "triage:end")
			return nil
		},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "transfer_to_billing", "h1", `{}`)),
		}},
	}

	if _, err := RunSync(context.Background(), triage, "help", RunOptions{}); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(order, ",")
	// Triage starts, hands off, billing starts and finishes. Triage never
	// "ends" because it did not produce the final output — the agent that did
	// is the one that ends.
	want := "triage:start,billing:start,billing:end"
	if got != want {
		t.Errorf("callbacks = %q, want %q", got, want)
	}
}

// A callback that fails aborts the run rather than being swallowed.
func TestAgentCallbacks_ErrorAbortsTheRun(t *testing.T) {
	boom := errors.New("not allowed to start")
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "never"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		OnStart:   func(context.Context, *RunContext) error { return boom },
	}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the callback's error", err)
	}
	if model.calls != 0 {
		t.Errorf("the model was called %d times after OnStart failed", model.calls)
	}
}

// What the removed hooks observed is now on the stream — and unlike a hook,
// a consumer can read the raw model events too.
func TestStreamReplacesTheObservationHooks(t *testing.T) {
	tool := NewFunctionTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "out", nil })
	billing := &Agent{Name: "billing", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "handled")),
	}}}
	triage := &Agent{
		Name: "triage", Tools: []Tool{tool}, Handoffs: []Handoff{HandoffTo(billing)},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "t", "c1", `{}`)),
			modelResp(functionCallOutput(t, "transfer_to_billing", "h1", `{}`)),
		}},
	}

	stream, _ := Run(context.Background(), triage, "go", RunOptions{})
	events, res, err := streamRun(stream)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("no result")
	}

	var agentSwitches int
	seen := map[string]bool{}
	for _, ev := range events {
		switch e := ev.(type) {
		case *AgentUpdatedStreamEvent:
			agentSwitches++
		case *RunItemStreamEvent:
			seen[e.Name] = true
		}
	}
	// OnAgentStart / OnHandoff become agent-updated events.
	if agentSwitches != 2 {
		t.Errorf("agent switches = %d, want 2 (start + handoff)", agentSwitches)
	}
	// OnToolStart / OnToolEnd become item events.
	for _, want := range []string{"tool_called", "tool_output", "handoff_requested"} {
		if !seen[want] {
			t.Errorf("event %q missing; it replaced a hook", want)
		}
	}
}

// The old tool hooks could only refuse. A tool-stage guardrail can also
// REWRITE, which is the capability the removal gained rather than lost.
func TestToolGuardrailReplacesToolHooks(t *testing.T) {
	tool := NewFunctionTool("t", "t",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "raw output", nil })
	tool.Guardrails = []Guardrail{{
		Name:   "rewrite",
		Stages: []GuardrailStage{StageToolOutput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			return Replace("rewritten by guardrail", nil), nil
		},
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "t", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := findToolOutput(res.NewItems)
	if out == nil {
		t.Fatal("no tool output")
	}
	if got := stringifyToolOutput(out.Output); got != "rewritten by guardrail" {
		t.Errorf("tool output = %q; a guardrail should be able to rewrite, not only refuse", got)
	}
}
