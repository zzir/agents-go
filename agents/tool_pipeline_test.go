package agents

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A handled tool error (FailureErrorFunction) still produces a tool-output item
// and runs through output guardrails — the error message is a normal result.
func TestToolPipeline_HandledErrorStillProducesOutput(t *testing.T) {
	var guardrailSawOutput string
	tool := NewTool("boom", "fails",
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
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

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
	tool := NewTool("gated", "does stuff",
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
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

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

// A struct-returning tool on a plain-text agent yields a STRING final output,
// not a raw Go value, when a turn hook stops the run on its result.
func TestToolPipeline_StoppedTurnStringifiesForPlainText(t *testing.T) {
	type payload struct {
		N int `json:"n"`
	}
	tool := NewTool("compute", "computes",
		func(ctx context.Context, tc *ToolContext, args struct{}) (payload, error) {
			return payload{N: 42}, nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{ShouldStopAfterTurn: stopAlways},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.FinalOutput.(string); !ok {
		t.Errorf("final output type = %T, want string (plain-text agent coerces)", res.FinalOutput)
	}
}

// A rejected tool call still produces an output item and keeps call order, so a
// turn hook that stops the run reports the rejection message as the final
// output rather than an empty one.
func TestToolPipeline_RejectedCallParticipatesInTurnResult(t *testing.T) {
	tool := NewTool("act", "acts",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "executed", nil
		})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "act", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	opts := RunOptions{Exec: ExecOptions{ShouldStopAfterTurn: stopAlways}}
	res, err := RunSync(context.Background(), agent, "go", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}
	// Reject, then resume: the rejected call is the only tool, so its rejection
	// message is what the stopped turn produced.
	res.State.Reject(res.Interruptions[0], false, "not allowed")
	res2, err := ResumeRunSync(context.Background(), res.State, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res2.FinalOutputString() != "not allowed" {
		t.Errorf("final = %q, want the rejection message (a rejected call is still a turn result)", res2.FinalOutputString())
	}
}

// When several tools fail fatally in one turn, the reported error is the
// lowest-call-index one, regardless of which goroutine finishes first.
func TestToolPipeline_ConcurrentFailureDeterministicWinner(t *testing.T) {
	// Tool "first" fails slowly; tool "second" fails fast. Without deterministic
	// arbitration the fast one would win; with it, "first" (lower index) wins.
	first := NewTool("first", "slow fail",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return "", errors.New("first-error")
		})
	first.FailureErrorFunction = nil // fatal
	second := NewTool("second", "fast fail",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "", errors.New("second-error")
		})
	second.FailureErrorFunction = nil // fatal

	model := &fakeModel{responses: []*ModelResponse{{
		Output: []OutputItem{
			functionCallOutput(t, "first", "c1", `{}`),
			functionCallOutput(t, "second", "c2", `{}`),
		},
		Usage: NewUsage(),
	}}}
	agent := &Agent{Name: "a", Tools: []*Tool{first, second}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "first-error") {
		t.Errorf("err = %v, want the lowest-index (first) tool's error", err)
	}
}

func TestToolInputGuardrailReject(t *testing.T) {
	tool := NewTool("danger", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
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
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The rejection message should appear as a tool output item.
	var found bool
	for _, it := range res.NewItems {
		if it.Kind == ItemToolCallOutput && it.Output == "not allowed" {
			found = true
		}
	}
	if !found {
		t.Error("expected rejected tool output 'not allowed' in items")
	}
}

func TestToolOutputGuardrailRaise(t *testing.T) {
	tool := NewTool("leaky", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
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
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if _, ok := errors.AsType[*GuardrailTripwireError](err); !ok {
		t.Fatalf("expected *GuardrailTripwireError, got %T (%v)", err, err)
	}
}

// Tool error is fed back to the model by default.
func TestToolError_FeedsBackToModel(t *testing.T) {
	tool := NewTool("boom", "fails", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "", errors.New("kaboom")
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

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
		if it.Kind == ItemToolCallOutput {
			if s, _ := it.Output.(string); strings.Contains(s, "kaboom") {
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
	tool := NewTool("boom", "fails", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "", errors.New("kaboom")
	})
	tool.FailureErrorFunction = nil // opt into raising
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err == nil {
		t.Fatal("expected run to fail when FailureErrorFunction is nil")
	}
}

// stopAlways is the turn hook for tests that only care about what a stopped run
// reports, not about when it stops.
func stopAlways(context.Context, *TurnResult) (bool, error) { return true, nil }

// ShouldStopAfterTurn replaces the agent-level tool-use-behavior callback. It
// is a predicate rather than a producer: the run reports what the turn actually
// produced, so a stopped run's final output cannot disagree with its history.
func TestShouldStopAfterTurn_StopsOnToolResult(t *testing.T) {
	tool := NewTool("calc", "", func(ctx context.Context, tc *ToolContext, a struct{}) (int, error) {
		return 42, nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "calc", "c1", `{}`)),
		modelResp(messageOutput(t, "never reached")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{ShouldStopAfterTurn: func(_ context.Context, tr *TurnResult) (bool, error) {
			return slices.Contains(tr.ToolCallNames(), "calc"), nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "42" {
		t.Errorf("final = %q, want the tool output", res.FinalOutputString())
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 (the hook stopped the run)", model.calls)
	}
}
