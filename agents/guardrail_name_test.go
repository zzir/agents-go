package agents

import (
	"context"
	"strings"
	"testing"
)

func trippingInput(name string) InputGuardrail {
	return InputGuardrail{
		Name: name,
		Run: func(context.Context, *RunContext, *Agent, []TResponseInputItem) (GuardrailFunctionOutput, error) {
			return GuardrailFunctionOutput{TripwireTriggered: true}, nil
		},
	}
}

func trippingOutput(name string) OutputGuardrail {
	return OutputGuardrail{
		Name: name,
		Run: func(context.Context, *RunContext, *Agent, any) (GuardrailFunctionOutput, error) {
			return GuardrailFunctionOutput{TripwireTriggered: true}, nil
		},
	}
}

func TestInputGuardrailNameFallback(t *testing.T) {
	_, err := runInputGuardrails(context.Background(), NewRunContext(nil), &Agent{Name: "a"},
		[]InputGuardrail{trippingInput("")}, nil)
	if err == nil || !strings.Contains(err.Error(), "input_guardrail") {
		t.Fatalf("unnamed input guardrail should use fallback name, got %v", err)
	}
	// An explicit name is preserved.
	_, err = runInputGuardrails(context.Background(), NewRunContext(nil), &Agent{Name: "a"},
		[]InputGuardrail{trippingInput("pii")}, nil)
	if err == nil || !strings.Contains(err.Error(), "pii") {
		t.Fatalf("named input guardrail should keep its name, got %v", err)
	}
}

func TestOutputGuardrailNameFallback(t *testing.T) {
	_, err := runOutputGuardrails(context.Background(), NewRunContext(nil), &Agent{Name: "a"},
		[]OutputGuardrail{trippingOutput("")}, "out")
	if err == nil || !strings.Contains(err.Error(), "output_guardrail") {
		t.Fatalf("unnamed output guardrail should use fallback name, got %v", err)
	}
}

func TestGuardrailResolvedName(t *testing.T) {
	if got := (InputGuardrail{}).resolvedName(); got != "input_guardrail" {
		t.Fatalf("input default = %q", got)
	}
	if got := (InputGuardrail{Name: "x"}).resolvedName(); got != "x" {
		t.Fatalf("input explicit = %q", got)
	}
	if got := (OutputGuardrail{}).resolvedName(); got != "output_guardrail" {
		t.Fatalf("output default = %q", got)
	}
}
