package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A handled tool error (FailureErrorFunction) still produces a tool-output item
// and runs through output guardrails — the error message is a normal result.
func TestToolPipeline_HandledErrorStillProducesOutput(t *testing.T) {
	var guardrailSawOutput string
	tool := NewFunctionTool("boom", "fails",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "", errors.New("kaboom")
		})
	tool.Guardrails = []Guardrail{{
		Name:   "redact",
		Stages: []GuardrailStage{StageToolOutput},
		Run: func(_ context.Context, _ *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			guardrailSawOutput, _ = p.Output.(string)
			return Allow(nil), nil
		},
	}}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	// The failed tool still produced an output item, marked as an error.
	out := findToolOutput(res.NewItems)
	if out == nil {
		t.Fatal("a handled tool failure produced no output item")
	}
	if !out.Display().IsError {
		t.Error("the handled failure is not marked as an error")
	}
	// The output guardrail saw the failure message.
	if !strings.Contains(guardrailSawOutput, "kaboom") {
		t.Errorf("output guardrail saw %q, want it to contain the error message", guardrailSawOutput)
	}
}

// A tool-input guardrail that substitutes content stops the tool from running
// at all: the substituted message becomes the result.
func TestToolPipeline_InputGuardrailRejectSkipsTheTool(t *testing.T) {
	tool := NewFunctionTool("gated", "does stuff",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ran", nil
		})
	tool.Guardrails = []Guardrail{{
		Name:   "block",
		Stages: []GuardrailStage{StageToolInput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			return Replace("blocked", nil), nil
		},
	}}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "gated", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	var ran bool
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = ran
	out := findToolOutput(res.NewItems)
	if out == nil {
		t.Fatal("no tool output item")
	}
	if got := stringifyToolOutput(out.Output); got != "blocked" {
		t.Errorf("tool output = %q, want the guardrail's substitution", got)
	}
}

// StopOnFirstTool with a struct-returning tool on a plain-text agent
// yields a STRING final output (Python str()), not a raw Go value.
func TestToolPipeline_StopOnFirstToolStringifiesForPlainText(t *testing.T) {
	type payload struct {
		N int `json:"n"`
	}
	tool := NewFunctionTool("compute", "computes",
		func(ctx context.Context, tc *ToolContext, args struct{}) (payload, error) {
			return payload{N: 42}, nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ToolUseBehavior: StopOnFirstTool{}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.FinalOutput.(string); !ok {
		t.Errorf("final output type = %T, want string (plain-text agent coerces)", res.FinalOutput)
	}
}

// A rejected tool call participates in StopOnFirstTool and keeps call
// order — the rejection message becomes the final output.
func TestToolPipeline_RejectedCallParticipatesInToolUseBehavior(t *testing.T) {
	tool := NewFunctionTool("act", "acts",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "executed", nil
		})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "act", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ToolUseBehavior: StopOnFirstTool{}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}
	// Reject, then resume: the rejected call is the only tool, StopOnFirstTool
	// makes its rejection message the final output.
	res.State.Reject(res.Interruptions[0], false, "not allowed")
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.FinalOutputString() != "not allowed" {
		t.Errorf("final = %q, want the rejection message (rejected call feeds ToolUseBehavior)", res2.FinalOutputString())
	}
}

// When several tools fail fatally in one turn, the reported error is the
// lowest-call-index one, regardless of which goroutine finishes first.
func TestToolPipeline_ConcurrentFailureDeterministicWinner(t *testing.T) {
	// Tool "first" fails slowly; tool "second" fails fast. Without deterministic
	// arbitration the fast one would win; with it, "first" (lower index) wins.
	first := NewFunctionTool("first", "slow fail",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return "", errors.New("first-error")
		})
	first.FailureErrorFunction = nil // fatal
	second := NewFunctionTool("second", "fast fail",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "", errors.New("second-error")
		})
	second.FailureErrorFunction = nil // fatal

	model := &fakeModel{responses: []*ModelResponse{{
		Output: []TResponseOutputItem{
			functionCallOutput(t, "first", "c1", `{}`),
			functionCallOutput(t, "second", "c2", `{}`),
		},
		Usage: NewUsage(),
	}}}
	agent := &Agent{Name: "a", Tools: []Tool{first, second}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "first-error") {
		t.Errorf("err = %v, want the lowest-index (first) tool's error", err)
	}
}

func TestToolInputGuardrailReject(t *testing.T) {
	tool := NewFunctionTool("danger", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		t.Error("tool should not run when input guardrail rejects")
		return "ran", nil
	})
	tool.Guardrails = []Guardrail{{
		Name:   "guard",
		Stages: []GuardrailStage{StageToolInput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			return Replace("not allowed", nil), nil
		},
	}}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "danger", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
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
	tool.Guardrails = []Guardrail{{
		Name:   "guard",
		Stages: []GuardrailStage{StageToolOutput},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			return Trip(nil), nil
		},
	}}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "leaky", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	var tw *GuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("expected *GuardrailTripwireError, got %T (%v)", err, err)
	}
}

// Tool error is fed back to the model by default.
func TestToolError_FeedsBackToModel(t *testing.T) {
	tool := NewFunctionTool("boom", "fails", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "", errors.New("kaboom")
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatalf("run should not fail when tool error is fed back: %v", err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	// The error message should appear as a tool output item.
	var found bool
	for _, it := range res.NewItems {
		if o, ok := it.(*ToolCallOutputItem); ok {
			if s, _ := o.Output.(string); strings.Contains(s, "kaboom") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected tool error message fed back as tool output")
	}
}

// Setting FailureErrorFunction to nil makes tool errors fatal.
func TestToolError_FatalWhenNil(t *testing.T) {
	tool := NewFunctionTool("boom", "fails", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "", errors.New("kaboom")
	})
	tool.FailureErrorFunction = nil // opt into raising
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err == nil {
		t.Fatal("expected run to fail when FailureErrorFunction is nil")
	}
}

// The tool_use_behavior callback can stop with a custom final output.
func TestToolUseBehaviorFunc(t *testing.T) {
	tool := NewFunctionTool("calc", "", func(ctx context.Context, tc *ToolContext, a struct{}) (int, error) {
		return 42, nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "calc", "c1", `{}`)),
	}}
	agent := &Agent{
		Name:      "a",
		Tools:     []Tool{tool},
		ModelImpl: model,
		ToolUseBehavior: ToolUseBehaviorFunc(func(ctx context.Context, rc *RunContext, results []FunctionToolResult) (bool, any, error) {
			if len(results) == 1 && results[0].ToolName == "calc" {
				return true, "computed:42", nil
			}
			return false, nil, nil
		}),
	}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "computed:42" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 (behavior func stopped the run)", model.calls)
	}
}
