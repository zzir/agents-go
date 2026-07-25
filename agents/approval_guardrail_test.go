package agents

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
)

// --- #3229/#3259 parity: an already-resolved approval decision must not
// re-invoke the NeedsApproval checker on resume. ---

func TestApproval_CheckerSkippedWhenDecisionResolved(t *testing.T) {
	var checks, ran atomic.Int32
	tool := NewFunctionTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			ran.Add(1)
			return "deleted", nil
		})
	tool.NeedsApprovalFunc = func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error) {
		checks.Add(1)
		return true, nil
	}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "call_1", `{}`)),
		modelResp(messageOutput(t, "all done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}
	if got := checks.Load(); got != 1 {
		t.Fatalf("checker calls before resume = %d, want 1", got)
	}

	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRun(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := checks.Load(); got != 1 {
		t.Errorf("checker re-invoked on resume: calls = %d, want 1", got)
	}
	if ran.Load() != 1 {
		t.Errorf("tool runs = %d, want 1", ran.Load())
	}
	if res2.FinalOutputString() != "all done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}

func TestApproval_CheckerErrorNotRaisedForApprovedCall(t *testing.T) {
	// After approval, a checker that would now fail must not fire at all.
	var calls atomic.Int32
	tool := NewFunctionTool("deploy", "deploys",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ok", nil
		})
	tool.NeedsApprovalFunc = func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error) {
		if calls.Add(1) > 1 {
			return false, errors.New("checker exploded on second call")
		}
		return true, nil
	}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "deploy", "call_1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res.State.Approve(res.Interruptions[0], false)
	if _, err := ResumeRun(context.Background(), res.State, RunOptions{}); err != nil {
		t.Fatalf("resume failed (checker was re-invoked?): %v", err)
	}
}

// --- #3487 parity: pre-approval tool input guardrails. ---

func preApprovalFixture(t *testing.T, guardrail Guardrail, ran *atomic.Int32) *Agent {
	t.Helper()
	tool := NewFunctionTool("send_mail", "sends mail",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			ran.Add(1)
			return "sent", nil
		})
	tool.NeedsApproval = true
	tool.Guardrails = []Guardrail{guardrail}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "send_mail", "call_1", `{}`)),
		modelResp(messageOutput(t, "understood")),
	}}
	return &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}
}

func TestPreApprovalGuardrail_RejectSkipsApprovalAndExecution(t *testing.T) {
	var ran, guardrailRuns atomic.Int32
	g := Guardrail{
		Name:   "block",
		Stages: []GuardrailStage{StageToolInput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			guardrailRuns.Add(1)
			return Replace("blocked by policy", nil), nil
		},
	}
	agent := preApprovalFixture(t, g, &ran)

	res, err := Run(context.Background(), agent, "send", RunOptions{PreApprovalToolInputGuardrails: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 0 {
		t.Fatalf("expected no interruptions, got %d", len(res.Interruptions))
	}
	if ran.Load() != 0 {
		t.Error("tool must not execute when pre-approval guardrail rejects")
	}
	if guardrailRuns.Load() != 1 {
		t.Errorf("guardrail runs = %d, want 1", guardrailRuns.Load())
	}
	var found bool
	for _, it := range res.NewItems {
		if o, ok := it.(*ToolCallOutputItem); ok && o.Output == "blocked by policy" {
			found = true
		}
	}
	if !found {
		t.Error("expected guardrail message as tool output")
	}
	if res.FinalOutputString() != "understood" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

func TestPreApprovalGuardrail_PassStillInterruptsAndRerunsOnResume(t *testing.T) {
	var ran, guardrailRuns atomic.Int32
	g := Guardrail{
		Name:   "count",
		Stages: []GuardrailStage{StageToolInput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			guardrailRuns.Add(1)
			return Allow(nil), nil
		},
	}
	agent := preApprovalFixture(t, g, &ran)
	opts := RunOptions{PreApprovalToolInputGuardrails: true}

	res, err := Run(context.Background(), agent, "send", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}
	if guardrailRuns.Load() != 1 {
		t.Fatalf("guardrail runs before approval = %d, want 1", guardrailRuns.Load())
	}

	res.State.Approve(res.Interruptions[0], false)
	if _, err := ResumeRun(context.Background(), res.State, opts); err != nil {
		t.Fatal(err)
	}
	// Passing calls revalidate the same guardrails right before execution.
	if guardrailRuns.Load() != 2 {
		t.Errorf("guardrail runs after resume = %d, want 2", guardrailRuns.Load())
	}
	if ran.Load() != 1 {
		t.Errorf("tool runs = %d, want 1", ran.Load())
	}
}

func TestPreApprovalGuardrail_OffByDefault(t *testing.T) {
	var ran, guardrailRuns atomic.Int32
	g := Guardrail{
		Name:   "block",
		Stages: []GuardrailStage{StageToolInput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			guardrailRuns.Add(1)
			return Replace("blocked", nil), nil
		},
	}
	agent := preApprovalFixture(t, g, &ran)

	res, err := Run(context.Background(), agent, "send", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption with flag off, got %d", len(res.Interruptions))
	}
	if guardrailRuns.Load() != 0 {
		t.Errorf("guardrail must not run pre-approval with flag off, ran %d times", guardrailRuns.Load())
	}
}

func TestPreApprovalGuardrail_TripwireHaltsRun(t *testing.T) {
	var ran atomic.Int32
	g := Guardrail{
		Name:   "trip",
		Stages: []GuardrailStage{StageToolInput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			return Trip(nil), nil
		},
	}
	agent := preApprovalFixture(t, g, &ran)

	_, err := Run(context.Background(), agent, "send", RunOptions{PreApprovalToolInputGuardrails: true})
	var tripErr *GuardrailTripwireError
	if !errors.As(err, &tripErr) {
		t.Fatalf("expected *GuardrailTripwireError, got %v", err)
	}
	if ran.Load() != 0 {
		t.Error("tool must not run after tripwire")
	}
}

// --- #3486/#3657 parity: SDK-only custom data for tool outputs. ---

func customDataAgent(t *testing.T, extractor func(ctx context.Context, cdc FunctionToolCustomDataContext) (map[string]any, error)) *Agent {
	t.Helper()
	tool := NewFunctionTool("get_data", "returns data",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "tool_result", nil
		})
	tool.CustomDataExtractor = extractor
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_data", "call_1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	return &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}
}

func findToolOutput(items []RunItem) *ToolCallOutputItem {
	for _, it := range items {
		if o, ok := it.(*ToolCallOutputItem); ok {
			return o
		}
	}
	return nil
}

func TestCustomData_AttachedToRunItemNotModel(t *testing.T) {
	agent := customDataAgent(t, func(ctx context.Context, cdc FunctionToolCustomDataContext) (map[string]any, error) {
		if cdc.Output != "tool_result" {
			t.Errorf("extractor Output = %v", cdc.Output)
		}
		if cdc.Tool == nil || cdc.Tool.Name != "get_data" {
			t.Error("extractor Tool not populated")
		}
		if cdc.ToolContext == nil || cdc.ToolContext.ToolCallID != "call_1" {
			t.Error("extractor ToolContext not populated")
		}
		return map[string]any{"renderer": "table", "id": 7}, nil
	})

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := findToolOutput(res.NewItems)
	if out == nil {
		t.Fatal("no ToolCallOutputItem in NewItems")
	}
	if out.CustomData["renderer"] != "table" {
		t.Errorf("CustomData = %v", out.CustomData)
	}
	// json.Unmarshal turns numbers into float64 — the JSON round-trip contract.
	if out.CustomData["id"] != float64(7) {
		t.Errorf("CustomData id = %v (%T)", out.CustomData["id"], out.CustomData["id"])
	}
	// Never part of the replayed input item.
	in, err := out.ToInputItem()
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := in.MarshalJSON(); err == nil && strings.Contains(string(raw), "renderer") {
		t.Error("custom data leaked into the model-visible input item")
	}
}

func TestCustomData_EmptyAndNilNormalizeToNil(t *testing.T) {
	agent := customDataAgent(t, func(ctx context.Context, cdc FunctionToolCustomDataContext) (map[string]any, error) {
		return map[string]any{}, nil
	})
	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out := findToolOutput(res.NewItems); out == nil || out.CustomData != nil {
		t.Errorf("empty map should normalize to nil, got %v", out.CustomData)
	}
}

func TestCustomData_NonJSONCompatibleFailsRun(t *testing.T) {
	for name, value := range map[string]any{
		"nan":  math.NaN(),
		"inf":  math.Inf(1),
		"chan": make(chan int),
	} {
		t.Run(name, func(t *testing.T) {
			agent := customDataAgent(t, func(ctx context.Context, cdc FunctionToolCustomDataContext) (map[string]any, error) {
				return map[string]any{"bad": value}, nil
			})
			_, err := Run(context.Background(), agent, "go", RunOptions{})
			var uerr *UserError
			if !errors.As(err, &uerr) {
				t.Fatalf("expected UserError, got %v", err)
			}
		})
	}
}

func TestCustomData_ExtractorErrorAbortsRun(t *testing.T) {
	agent := customDataAgent(t, func(ctx context.Context, cdc FunctionToolCustomDataContext) (map[string]any, error) {
		return nil, errors.New("boom")
	})
	if _, err := Run(context.Background(), agent, "go", RunOptions{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected extractor error to abort the run, got %v", err)
	}
}

func TestCustomData_SurvivesRunStateRoundTrip(t *testing.T) {
	tool := NewFunctionTool("get_data", "returns data",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "tool_result", nil
		})
	tool.CustomDataExtractor = func(ctx context.Context, cdc FunctionToolCustomDataContext) (map[string]any, error) {
		return map[string]any{"k": "v"}, nil
	}
	gated := NewFunctionTool("guarded", "needs ok",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "fine", nil
		})
	gated.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		// Turn 1: the extractor tool runs, then a guarded call interrupts.
		modelResp(functionCallOutput(t, "get_data", "call_1", `{}`)),
		modelResp(functionCallOutput(t, "guarded", "call_2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool, gated}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected interruption, got %d", len(res.Interruptions))
	}

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"custom_data"`) {
		t.Fatal("serialized state missing custom_data")
	}
	state, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	out := findToolOutput(state.GeneratedItems)
	if out == nil {
		t.Fatal("restored state lost the typed tool output item")
	}
	if out.CustomData["k"] != "v" {
		t.Errorf("restored CustomData = %v", out.CustomData)
	}

	state.Approve(state.Interruptions[0], false)
	res2, err := ResumeRun(context.Background(), state, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.FinalOutputString() != "done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}
