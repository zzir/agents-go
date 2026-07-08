package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

// Every executed tool guardrail — including a non-tripping "allow" — is
// surfaced on RunResult.Tool{Input,Output}GuardrailResults with its OutputInfo.
func TestGuardrailResults_ToolGuardrailsSurfacedOnResult(t *testing.T) {
	tool := NewFunctionTool("act", "acts",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "done", nil
		})
	tool.InputGuardrails = []ToolInputGuardrail{{
		Name: "in_gr",
		Run: func(ctx context.Context, rc *RunContext, data ToolInputGuardrailData) (ToolGuardrailFunctionOutput, error) {
			return AllowTool("in-info"), nil
		},
	}}
	tool.OutputGuardrails = []ToolOutputGuardrail{{
		Name: "out_gr",
		Run: func(ctx context.Context, rc *RunContext, data ToolOutputGuardrailData) (ToolGuardrailFunctionOutput, error) {
			return AllowTool("out-info"), nil
		},
	}}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "act", "c1", `{}`)),
		modelResp(messageOutput(t, "final")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolInputGuardrailResults) != 1 {
		t.Fatalf("ToolInputGuardrailResults = %d, want 1", len(res.ToolInputGuardrailResults))
	}
	if got := res.ToolInputGuardrailResults[0]; got.Guardrail.Name != "in_gr" ||
		got.ToolName != "act" || got.ToolCallID != "c1" || got.Output.OutputInfo != "in-info" {
		t.Errorf("input result = %+v", got)
	}
	if len(res.ToolOutputGuardrailResults) != 1 {
		t.Fatalf("ToolOutputGuardrailResults = %d, want 1", len(res.ToolOutputGuardrailResults))
	}
	if got := res.ToolOutputGuardrailResults[0]; got.Guardrail.Name != "out_gr" ||
		got.ToolName != "act" || got.Output.OutputInfo != "out-info" {
		t.Errorf("output result = %+v", got)
	}
}

// A failed run's RunErrorDetails carries the input and output guardrail
// results accumulated before the failure (Python parity: exceptions.py).
func TestGuardrailResults_RunErrorDetailsCarriesGuardrailResults(t *testing.T) {
	agent := &Agent{
		Name:      "a",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hi"))}},
		InputGuardrails: []InputGuardrail{{
			Name:     "in_gr",
			Blocking: true, // deterministic: recorded before the model call
			Run: func(ctx context.Context, rc *RunContext, agent *Agent, input []TResponseInputItem) (GuardrailFunctionOutput, error) {
				return GuardrailFunctionOutput{OutputInfo: "in-info"}, nil
			},
		}},
		OutputGuardrails: []OutputGuardrail{{
			Name: "out_gr",
			Run: func(ctx context.Context, rc *RunContext, agent *Agent, output any) (GuardrailFunctionOutput, error) {
				return GuardrailFunctionOutput{OutputInfo: "out-info", TripwireTriggered: true}, nil
			},
		}},
	}

	_, err := Run(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatal("expected the output guardrail tripwire to fail the run")
	}
	ae, ok := AsAgentsError(err)
	if !ok || ae.Details == nil {
		t.Fatalf("no RunErrorDetails on error %v", err)
	}
	if len(ae.Details.InputGuardrailResults) != 1 || ae.Details.InputGuardrailResults[0].Output.OutputInfo != "in-info" {
		t.Errorf("Details.InputGuardrailResults = %+v", ae.Details.InputGuardrailResults)
	}
	if len(ae.Details.OutputGuardrailResults) != 1 || ae.Details.OutputGuardrailResults[0].Output.OutputInfo != "out-info" {
		t.Errorf("Details.OutputGuardrailResults = %+v", ae.Details.OutputGuardrailResults)
	}
}

// Guardrail results are carried on the interruption RunState and survive a
// JSON round-trip (lossily: name + output payload), so a resumed run can still
// report them.
func TestGuardrailResults_RunStateRoundTripPreservesGuardrailResults(t *testing.T) {
	tool := NewFunctionTool("delete_db", "deletes",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "deleted", nil
		})
	tool.NeedsApproval = true
	agent := &Agent{
		Name:      "a",
		Tools:     []Tool{tool},
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "delete_db", "c1", `{}`))}},
		InputGuardrails: []InputGuardrail{{
			Name:     "in_gr",
			Blocking: true,
			Run: func(ctx context.Context, rc *RunContext, agent *Agent, input []TResponseInputItem) (GuardrailFunctionOutput, error) {
				return GuardrailFunctionOutput{OutputInfo: map[string]any{"score": 0.5}}, nil
			},
		}},
	}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.State == nil {
		t.Fatal("expected a paused RunState")
	}
	if len(res.State.InputGuardrailResults) != 1 || res.State.InputGuardrailResults[0].Guardrail.Name != "in_gr" {
		t.Fatalf("State.InputGuardrailResults = %+v", res.State.InputGuardrailResults)
	}

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	if len(rebuilt.InputGuardrailResults) != 1 {
		t.Fatalf("rebuilt.InputGuardrailResults = %d, want 1", len(rebuilt.InputGuardrailResults))
	}
	got := rebuilt.InputGuardrailResults[0]
	if got.Guardrail.Name != "in_gr" {
		t.Errorf("rebuilt guardrail name = %q, want in_gr", got.Guardrail.Name)
	}
	// OutputInfo round-trips through JSON, so the concrete map becomes map[string]any.
	m, ok := got.Output.OutputInfo.(map[string]any)
	if !ok || m["score"] != 0.5 {
		t.Errorf("rebuilt OutputInfo = %#v, want {score:0.5}", got.Output.OutputInfo)
	}
}

// A streamed final Response with no usage block counts as zero requests,
// matching the blocking path (Python's Usage() fallback).
func TestGuardrailResults_StreamUsageAbsentIsZeroRequests(t *testing.T) {
	var noUsage responses.Response
	if err := json.Unmarshal([]byte(`{"id":"r","output":[]}`), &noUsage); err != nil {
		t.Fatal(err)
	}
	if u := usageFromStreamResponse(&noUsage); u.Requests != 0 {
		t.Errorf("no-usage stream: Requests = %d, want 0", u.Requests)
	}

	var withUsage responses.Response
	if err := json.Unmarshal([]byte(`{"id":"r","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`), &withUsage); err != nil {
		t.Fatal(err)
	}
	if u := usageFromStreamResponse(&withUsage); u.Requests != 1 || u.TotalTokens != 8 {
		t.Errorf("with-usage stream: Requests=%d Total=%d, want 1/8", u.Requests, u.TotalTokens)
	}
}

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

// Run-level guardrails run in addition to agent-level ones, and every
// guardrail's result (including non-tripping) is exposed on the RunResult.
func TestRunLevelGuardrails_MergeAndExposeResults(t *testing.T) {
	agentGuard := OutputGuardrail{
		Name: "agent-og",
		Run: func(_ context.Context, _ *RunContext, _ *Agent, _ any) (GuardrailFunctionOutput, error) {
			return GuardrailFunctionOutput{OutputInfo: "agent-checked"}, nil
		},
	}
	runInputGuard := InputGuardrail{
		Name: "run-ig",
		Run: func(_ context.Context, _ *RunContext, _ *Agent, _ []TResponseInputItem) (GuardrailFunctionOutput, error) {
			return GuardrailFunctionOutput{OutputInfo: "input-checked"}, nil
		},
	}
	runOutputGuard := OutputGuardrail{
		Name: "run-og",
		Run: func(_ context.Context, _ *RunContext, _ *Agent, _ any) (GuardrailFunctionOutput, error) {
			return GuardrailFunctionOutput{OutputInfo: "run-checked"}, nil
		},
	}
	agent := &Agent{
		Name:             "a",
		ModelImpl:        &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hi"))}},
		OutputGuardrails: []OutputGuardrail{agentGuard},
	}

	res, err := Run(context.Background(), agent, "go", RunOptions{
		InputGuardrails:  []InputGuardrail{runInputGuard},
		OutputGuardrails: []OutputGuardrail{runOutputGuard},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.InputGuardrailResults) != 1 || res.InputGuardrailResults[0].Output.OutputInfo != "input-checked" {
		t.Errorf("input guardrail results = %+v", res.InputGuardrailResults)
	}
	// Both the run-level and the agent-level output guardrails ran.
	if len(res.OutputGuardrailResults) != 2 {
		t.Fatalf("output guardrail results = %d, want 2 (run-level + agent-level)", len(res.OutputGuardrailResults))
	}
	// Non-tripping OutputInfo and the checked output/agent are exposed.
	if res.OutputGuardrailResults[0].AgentOutput != "hi" || res.OutputGuardrailResults[0].Agent != agent {
		t.Errorf("output guardrail result missing agent/output: %+v", res.OutputGuardrailResults[0])
	}
}

// An input guardrail with Blocking=true runs before the model call,
// so a tripwire prevents the call entirely.
func TestRunLevelGuardrails_SequentialBlocksModel(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hi"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := Run(context.Background(), agent, "go", RunOptions{
		InputGuardrails: []InputGuardrail{{
			Name:     "gate",
			Blocking: true, // run before the model call
			Run: func(_ context.Context, _ *RunContext, _ *Agent, _ []TResponseInputItem) (GuardrailFunctionOutput, error) {
				return GuardrailFunctionOutput{TripwireTriggered: true}, nil
			},
		}},
	})
	var tw *InputGuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("err = %v, want InputGuardrailTripwireError", err)
	}
	if model.calls != 0 {
		t.Errorf("model called %d times; a sequential guardrail tripwire must block the call", model.calls)
	}
}
