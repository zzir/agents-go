package agents

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

// resultsFor returns the guardrail results recorded for one stage.
func resultsFor(rs []GuardrailResult, stage GuardrailStage) []GuardrailResult {
	var out []GuardrailResult
	for _, r := range rs {
		if r.Stage == stage {
			out = append(out, r)
		}
	}
	return out
}

func TestGuardrailResults_ToolStagesSurfacedOnResult(t *testing.T) {
	tool := NewTool("echo", "echoes",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "echoed", nil
		})
	tool.Guardrails = []Guardrail{
		{
			Name:   "arg_check",
			Stages: []GuardrailStage{StageToolInput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Allow(map[string]any{"checked": true}), nil
			},
		},
		{
			Name:   "out_check",
			Stages: []GuardrailStage{StageToolOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Allow("clean"), nil
			},
		},
	}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "echo", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	in := resultsFor(res.GuardrailResults, StageToolInput)
	if len(in) != 1 || in[0].Guardrail.Name != "arg_check" {
		t.Fatalf("tool-input results = %+v", in)
	}
	if in[0].ToolName != "echo" || in[0].ToolCallID != "c1" {
		t.Errorf("tool-input result identity = %q/%q, want echo/c1", in[0].ToolName, in[0].ToolCallID)
	}
	out := resultsFor(res.GuardrailResults, StageToolOutput)
	if len(out) != 1 || out[0].Guardrail.Name != "out_check" {
		t.Fatalf("tool-output results = %+v", out)
	}
	// Allowing decisions are recorded too, so callers can read OutputInfo.
	if out[0].Decision.OutputInfo != "clean" {
		t.Errorf("output info = %#v, want clean", out[0].Decision.OutputInfo)
	}
}

// One guardrail covering several stages is consulted at each of them — the
// case that previously required four separate guardrail types.
func TestGuardrailResults_OneGuardrailCoversManyStages(t *testing.T) {
	// Stages are recorded from different goroutines (the input guardrail races
	// the model call, tool stages run in the tool goroutines), so the recorder
	// takes a lock rather than relying on the run loop's sequencing.
	var mu sync.Mutex
	var stages []GuardrailStage
	scanner := Guardrail{
		Name:   "scanner",
		Stages: []GuardrailStage{StageInput, StageToolInput, StageToolOutput, StageOutput},
		Run: func(_ context.Context, _ *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			mu.Lock()
			stages = append(stages, p.Stage)
			mu.Unlock()
			return Allow(nil), nil
		},
	}
	tool := NewTool("echo", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "echoed", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "echo", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model, Guardrails: []Guardrail{scanner}}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []GuardrailStage{StageInput, StageToolInput, StageToolOutput, StageOutput} {
		if len(resultsFor(res.GuardrailResults, want)) != 1 {
			mu.Lock()
			seen := append([]GuardrailStage(nil), stages...)
			mu.Unlock()
			t.Errorf("stage %s: got %d results, want 1 (all: %v)", want, len(resultsFor(res.GuardrailResults, want)), seen)
		}
	}
}

func TestGuardrailResults_RunErrorCarriesResults(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "leak"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "pii",
			Stages: []GuardrailStage{StageOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Trip(map[string]any{"reason": "ssn"}), nil
			},
		}},
	}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	re, ok := errors.AsType[*RunError](err)
	if !ok {
		t.Fatalf("err = %T, want a *RunError carrying partial progress", err)
	}
	got := resultsFor(re.Result.GuardrailResults, StageOutput)
	if len(got) != 1 || got[0].Guardrail.Name != "pii" {
		t.Fatalf("partial-result guardrail results = %+v", re.Result.GuardrailResults)
	}
}

func TestGuardrailResults_RunStateRoundTrip(t *testing.T) {
	tool := NewTool("delete_db", "deletes",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "deleted", nil
		})
	tool.NeedsApproval = true
	agent := &Agent{
		Name:      "a",
		Tools:     []*Tool{tool},
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "delete_db", "c1", `{}`))}},
		Guardrails: []Guardrail{{
			Name:     "in_gr",
			Stages:   []GuardrailStage{StageInput},
			Blocking: true,
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Allow(map[string]any{"score": 0.5}), nil
			},
		}},
	}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.State == nil {
		t.Fatal("expected a paused RunState")
	}
	staged := resultsFor(res.State.GuardrailResults, StageInput)
	if len(staged) != 1 || staged[0].Guardrail.Name != "in_gr" {
		t.Fatalf("State.GuardrailResults = %+v", res.State.GuardrailResults)
	}

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	got := resultsFor(rebuilt.GuardrailResults, StageInput)
	if len(got) != 1 {
		t.Fatalf("rebuilt input-stage results = %d, want 1", len(got))
	}
	if got[0].Guardrail.Name != "in_gr" {
		t.Errorf("rebuilt guardrail name = %q, want in_gr", got[0].Guardrail.Name)
	}
	if got[0].Stage != StageInput {
		t.Errorf("rebuilt stage = %q, want %q", got[0].Stage, StageInput)
	}
	// OutputInfo round-trips through JSON, so the concrete map becomes map[string]any.
	m, ok := got[0].Decision.OutputInfo.(map[string]any)
	if !ok || m["score"] != 0.5 {
		t.Errorf("rebuilt OutputInfo = %#v, want {score:0.5}", got[0].Decision.OutputInfo)
	}
}

// A streamed final Response with no usage block counts as zero requests,
// matching the blocking path.
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

func TestInputStageTripwire(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "block",
			Stages: []GuardrailStage{StageInput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Trip("nope"), nil
			},
		}},
	}
	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	var tw *GuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("err = %v (%T), want *GuardrailTripwireError", err, err)
	}
	if tw.Stage() != StageInput {
		t.Errorf("stage = %q, want %q", tw.Stage(), StageInput)
	}
	if tw.Result.Decision.OutputInfo != "nope" {
		t.Errorf("output info = %#v", tw.Result.Decision.OutputInfo)
	}
}

func TestOutputStageTripwire(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "secret"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "leak",
			Stages: []GuardrailStage{StageOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Trip(nil), nil
			},
		}},
	}
	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	var tw *GuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("err = %v (%T), want *GuardrailTripwireError", err, err)
	}
	if tw.Stage() != StageOutput {
		t.Errorf("stage = %q, want %q", tw.Stage(), StageOutput)
	}
	if tw.Result.Checked != "secret" {
		t.Errorf("checked value = %#v, want the final output", tw.Result.Checked)
	}
}

// Run-level and agent-level guardrails both apply.
//
// The callback must be goroutine-safe: guardrails at one stage run
// concurrently, so a shared counter needs synchronization.
func TestRunAndAgentGuardrailsBothApply(t *testing.T) {
	var ran atomic.Int32
	mk := func(name string) Guardrail {
		return Guardrail{
			Name:   name,
			Stages: []GuardrailStage{StageOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				ran.Add(1)
				return Allow(nil), nil
			},
		}
	}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hi"))}}
	agent := &Agent{Name: "a", ModelImpl: model, Guardrails: []Guardrail{mk("agent")}}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{Guardrails: []Guardrail{mk("run")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resultsFor(res.GuardrailResults, StageOutput)); got != 2 {
		t.Fatalf("output results = %d, want 2 (run-level + agent-level)", got)
	}
	if got := ran.Load(); got != 2 {
		t.Errorf("guardrails run = %d, want 2", got)
	}
}

// A Blocking input guardrail runs before the model call, so a tripwire
// prevents the call entirely.
func TestBlockingInputGuardrailPreventsModelCall(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hi"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{
		Guardrails: []Guardrail{{
			Name:     "gate",
			Stages:   []GuardrailStage{StageInput},
			Blocking: true,
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Trip(nil), nil
			},
		}},
	})
	var tw *GuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("err = %v, want *GuardrailTripwireError", err)
	}
	if model.calls != 0 {
		t.Errorf("model called %d times; a blocking guardrail tripwire must prevent the call", model.calls)
	}
}

// Replace at the output stage substitutes the final output and lets the run finish.
func TestOutputStageReplace(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "my ssn is 123"))}}
	agent := &Agent{
		Name:      "a",
		ModelImpl: model,
		Guardrails: []Guardrail{{
			Name:   "redact",
			Stages: []GuardrailStage{StageOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Replace("[redacted]", nil), nil
			},
		}},
	}
	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "[redacted]" {
		t.Errorf("final output = %q, want [redacted]", res.FinalOutputString())
	}
}
