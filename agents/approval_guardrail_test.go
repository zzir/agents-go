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
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
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
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
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
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res.State.Approve(res.Interruptions[0], false)
	if _, err := ResumeRunSync(context.Background(), res.State, RunOptions{}); err != nil {
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
	return &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}
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

	res, err := RunSync(context.Background(), agent, "send", RunOptions{Exec: ExecOptions{PreApprovalToolInputGuardrails: true}})
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
		if it.Kind == ItemToolCallOutput && it.Output == "blocked by policy" {
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
	opts := RunOptions{Exec: ExecOptions{PreApprovalToolInputGuardrails: true}}

	res, err := RunSync(context.Background(), agent, "send", opts)
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
	if _, err := ResumeRunSync(context.Background(), res.State, opts); err != nil {
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

	res, err := RunSync(context.Background(), agent, "send", RunOptions{})
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

	_, err := RunSync(context.Background(), agent, "send", RunOptions{Exec: ExecOptions{PreApprovalToolInputGuardrails: true}})
	var tripErr *GuardrailTripwireError
	if !errors.As(err, &tripErr) {
		t.Fatalf("expected *GuardrailTripwireError, got %v", err)
	}
	if ran.Load() != 0 {
		t.Error("tool must not run after tripwire")
	}
}

// --- SDK-only data on tool outputs, carried by ToolResult.Details. ---

// detailsAgent builds an agent whose single tool returns the given result.
func detailsAgent(t *testing.T, result func() (ToolResult, error)) *Agent {
	t.Helper()
	tool := NewFunctionTool("get_data", "returns data",
		func(ctx context.Context, tc *ToolContext, args struct{}) (ToolResult, error) {
			return result()
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_data", "call_1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	return &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: model}
}

func findToolOutput(items []*RunItem) *RunItem {
	for _, it := range items {
		if it.Kind == ItemToolCallOutput {
			return it
		}
	}
	return nil
}

// Details reach the run item and the display projection; they never reach the
// model. The tool declares them when it returns — there is no second extraction
// pass and nothing for a consumer to patch in afterwards.
func TestDetails_AttachedToRunItemNotModel(t *testing.T) {
	agent := detailsAgent(t, func() (ToolResult, error) {
		return TextResult("tool_result").
			WithDisplay("table").
			WithDetails(map[string]any{"renderer": "table", "id": 7}), nil
	})

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := findToolOutput(res.NewItems)
	if out == nil {
		t.Fatal("no ToolCallOutputItem in NewItems")
	}
	if out.Extra["renderer"] != "table" {
		t.Errorf("Extra = %v", out.Extra)
	}
	// json.Unmarshal turns numbers into float64 — the JSON round-trip contract.
	if out.Extra["id"] != float64(7) {
		t.Errorf("Extra id = %v (%T)", out.Extra["id"], out.Extra["id"])
	}
	d := out.Display()
	if d.Renderer != "table" {
		t.Errorf("Display().Renderer = %q, want the tool's hint", d.Renderer)
	}
	if d.Extra["renderer"] != "table" {
		t.Errorf("Display().Extra = %v", d.Extra)
	}
	if d.Output != "tool_result" {
		t.Errorf("Display().Output = %q", d.Output)
	}

	// Never part of the replayed input item.
	in, err := out.ToInputItem()
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := in.MarshalJSON(); err == nil && strings.Contains(string(raw), "renderer") {
		t.Error("details leaked into the model-visible input item")
	}
}

func TestDetails_EmptyNormalizesToNil(t *testing.T) {
	agent := detailsAgent(t, func() (ToolResult, error) {
		return TextResult("x").WithDetails(map[string]any{}), nil
	})
	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out := findToolOutput(res.NewItems); out == nil || out.Extra != nil {
		t.Errorf("an empty map should normalize to nil, got %v", out.Extra)
	}
}

// A value that cannot be serialized fails while the tool call is still
// identifiable, rather than at persistence time long after.
func TestDetails_NonJSONCompatibleFailsRun(t *testing.T) {
	for name, value := range map[string]any{
		"nan":  math.NaN(),
		"inf":  math.Inf(1),
		"chan": make(chan int),
	} {
		t.Run(name, func(t *testing.T) {
			agent := detailsAgent(t, func() (ToolResult, error) {
				return TextResult("x").WithDetails(map[string]any{"bad": value}), nil
			})
			_, err := RunSync(context.Background(), agent, "go", RunOptions{})
			var uerr *UserError
			if !errors.As(err, &uerr) {
				t.Fatalf("expected UserError, got %v", err)
			}
			if !strings.Contains(err.Error(), "get_data") {
				t.Errorf("the error should name the tool: %v", err)
			}
		})
	}
}

func TestDetails_SurviveRunStateRoundTrip(t *testing.T) {
	tool := NewFunctionTool("get_data", "returns data",
		func(ctx context.Context, tc *ToolContext, args struct{}) (ToolResult, error) {
			return TextResult("tool_result").WithDetails(map[string]any{"k": "v"}), nil
		})
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
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool, gated}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
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
	// The extractor's output now rides in the item's display projection rather
	// than a bespoke custom_data key, so a consumer reading a paused state gets
	// it from the same place it gets everything else it renders.
	if !strings.Contains(string(data), `"extra"`) {
		t.Fatalf("serialized state lost the extractor's data: %s", data)
	}
	// Provenance survives too: without it, a resumed run reports every restored
	// item as a plain model output.
	if !strings.Contains(string(data), `"type":"tool"`) {
		t.Errorf("serialized state lost the tool output's source: %s", data)
	}
	state, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	out := findToolOutput(state.GeneratedItems)
	if out == nil {
		t.Fatal("restored state lost the typed tool output item")
	}
	if out.Extra["k"] != "v" {
		t.Errorf("restored Extra = %v", out.Extra)
	}
	if got := out.Display().Extra["k"]; got != "v" {
		t.Errorf("Display().Extra = %v, want the extractor's data", got)
	}
	if src := out.Source; src.Type != SourceTool {
		t.Errorf("restored tool output source = %v, want tool", src)
	}

	state.Approve(state.Interruptions[0], false)
	res2, err := ResumeRunSync(context.Background(), state, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.FinalOutputString() != "done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}
