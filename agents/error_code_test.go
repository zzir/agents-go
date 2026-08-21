package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// CodeOf is the accessor transports use, so it must see through %w wrapping —
// the run loop wraps every tool error as `tool %q failed: %w`.
func TestCodeOf(t *testing.T) {
	tripwire := newTripwireError(GuardrailResult{
		Guardrail: Guardrail{Name: "g"}, Stage: StageInput,
	})
	cases := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{"nil", nil, CodeUnknown},
		{"plain error", errors.New("boom"), CodeUnknown},
		{"max turns", &MaxTurnsError{MaxTurns: 3}, CodeMaxTurns},
		{"model behavior", NewModelBehaviorError("bad"), CodeModelBehavior},
		{"user error", NewUserError("misuse"), CodeUserError},
		{"tripwire", tripwire, CodeGuardrailTripwire},
		{"wrapped once", fmt.Errorf("tool %q failed: %w", "t", &MaxTurnsError{MaxTurns: 3}), CodeMaxTurns},
		{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", tripwire)), CodeGuardrailTripwire},
		{"classified", Classify(CodeMCP, errors.New("connect refused")), CodeMCP},
		{"classified then wrapped", fmt.Errorf("x: %w", Classify(CodeSandboxExec, errors.New("exit 1"))), CodeSandboxExec},
		// Exported SDK types built as struct literals (no constructor) must
		// still classify, or a transport silently downgrades them to generic.
		{"struct-literal tripwire", &GuardrailTripwireError{}, CodeGuardrailTripwire},
		{"struct-literal max turns", &MaxTurnsError{MaxTurns: 1}, CodeMaxTurns},
		{"struct-literal refusal", &ModelRefusalError{Refusal: "no"}, CodeModelRefusal},
		{"struct-literal tool timeout", &ToolTimeoutError{ToolName: "t"}, CodeToolTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeOf(tc.err); got != tc.want {
				t.Errorf("CodeOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// Classify must not hide the error it tags: errors.Is/As still have to reach
// through it, or callers lose the ability to match sentinels.
func TestClassifyKeepsChain(t *testing.T) {
	if got := Classify(CodeMCP, nil); got != nil {
		t.Errorf("Classify(nil) = %v, want nil", got)
	}

	sentinel := errors.New("underlying")
	tagged := Classify(CodeMCP, fmt.Errorf("mcp: %w", sentinel))
	if !errors.Is(tagged, sentinel) {
		t.Error("errors.Is cannot reach the sentinel through Classify")
	}
	if CodeOf(tagged) != CodeMCP {
		t.Errorf("CodeOf = %q, want %q", CodeOf(tagged), CodeMCP)
	}

	// A typed SDK error stays matchable by type.
	tagged = Classify(CodeMCP, &MaxTurnsError{MaxTurns: 1})
	if _, ok := errors.AsType[*MaxTurnsError](tagged); !ok {
		t.Error("errors.As cannot reach *MaxTurnsError through Classify")
	}
}

// The innermost classification wins: a boundary that re-tags an already-coded
// error would replace the specific reason with its own generic one.
func TestClassifyDoesNotOverwrite(t *testing.T) {
	inner := &MaxTurnsError{MaxTurns: 2}
	if got := CodeOf(Classify(CodeMCP, inner)); got != CodeMaxTurns {
		t.Errorf("re-classifying overwrote the code: got %q, want %q", got, CodeMaxTurns)
	}
	// And it returns the original error untouched, not a wrapper.
	//nolint:errorlint // identity is the assertion: no wrapper was allocated
	if got := Classify(CodeMCP, inner); got != error(inner) {
		t.Errorf("Classify allocated a wrapper for an already-classified error: %T", got)
	}
}

// End to end: a code survives the run loop's own wrapping, which is the whole
// point — a transport reads it off the error Run returns.
func TestCodeSurvivesTheRunLoop(t *testing.T) {
	agent := &Agent{Name: "a", ModelImpl: &fakeModel{
		responses: []*ModelResponse{modelResp(functionCallOutput(t, "nope", "c1", `{}`))},
	}}
	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err == nil {
		t.Fatal("calling an undefined tool should fail the run")
	}
	if got := CodeOf(err); got != CodeModelBehavior {
		t.Errorf("CodeOf(run error) = %q, want %q (err: %v)", got, CodeModelBehavior, err)
	}

	_, err = RunSync(context.Background(), &Agent{Name: "a", ModelImpl: &fakeModel{
		responses: []*ModelResponse{modelResp(functionCallOutput(t, "loop", "c1", `{}`))},
	}, Tools: []*Tool{NewTool("loop", "loop",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "again", nil })}},
		"hi", RunOptions{Exec: ExecOptions{MaxTurns: 1}})
	if got := CodeOf(err); got != CodeMaxTurns {
		t.Errorf("CodeOf(max turns) = %q, want %q (err: %v)", got, CodeMaxTurns, err)
	}
}

// A tool panic aborts the run with CodeToolPanic, and the stack stays attached
// for the operator while the panic itself remains matchable.
//
// Only the fatal path is classified: with a FailureErrorFunction (which
// NewTool installs by default) a panic becomes tool output fed back to
// the model, and the run does not fail at all.
func TestToolPanicCode(t *testing.T) {
	panicking := &Tool{
		Name:             "boom",
		Description:      "boom",
		ParamsJSONSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false, "required": []any{}},
		Strict:           true,
		OnInvoke: func(context.Context, *ToolContext, string) (ToolResult, error) {
			panic("kaboom")
		},
		// nil FailureErrorFunction: the panic aborts the run.
	}
	agent := &Agent{Name: "a", ModelImpl: &fakeModel{
		responses: []*ModelResponse{modelResp(functionCallOutput(t, "boom", "c1", `{}`))},
	}, Tools: []*Tool{panicking}}

	_, err := RunSync(context.Background(), agent, "hi", RunOptions{})
	if err == nil {
		t.Fatal("a panicking tool should fail the run")
	}
	if got := CodeOf(err); got != CodeToolPanic {
		t.Errorf("CodeOf = %q, want %q (err: %v)", got, CodeToolPanic, err)
	}
	if _, ok := errors.AsType[*toolPanicError](err); !ok {
		t.Error("the panic itself is no longer reachable via errors.As")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("the panic value was dropped from the message: %v", err)
	}
}
