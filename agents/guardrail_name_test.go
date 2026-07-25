package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func tripping(name string, stage GuardrailStage) Guardrail {
	return Guardrail{
		Name:   name,
		Stages: []GuardrailStage{stage},
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			return Trip(nil), nil
		},
	}
}

// runStage is the test entry point for a single stage's concurrent execution.
func runStage(t *testing.T, stage GuardrailStage, gs ...Guardrail) error {
	t.Helper()
	_, err := runStageConcurrent(context.Background(), NewRunContext(nil), gs,
		GuardrailPayload{Stage: stage, Agent: &Agent{Name: "a"}})
	return err
}

func TestTripwireErrorNamesTheGuardrail(t *testing.T) {
	err := runStage(t, StageInput, tripping("", StageInput))
	if err == nil || !strings.Contains(err.Error(), "guardrail") {
		t.Fatalf("unnamed guardrail should use the fallback name, got %v", err)
	}
	err = runStage(t, StageInput, tripping("pii", StageInput))
	if err == nil || !strings.Contains(err.Error(), "pii") {
		t.Fatalf("named guardrail should keep its name, got %v", err)
	}
}

func TestTripwireErrorNamesTheStage(t *testing.T) {
	for _, stage := range []GuardrailStage{StageInput, StageOutput, StageToolInput, StageToolOutput} {
		err := runStage(t, stage, tripping("g", stage))
		if err == nil || !strings.Contains(err.Error(), string(stage)) {
			t.Errorf("%s tripwire error = %v, want it to name the stage", stage, err)
		}
		var tw *GuardrailTripwireError
		if !errors.As(err, &tw) {
			t.Fatalf("%s: err = %T, want *GuardrailTripwireError", stage, err)
		}
		if tw.Stage() != stage {
			t.Errorf("Stage() = %q, want %q", tw.Stage(), stage)
		}
	}
}

func TestGuardrailResolvedName(t *testing.T) {
	if got := (Guardrail{}).resolvedName(); got != "guardrail" {
		t.Fatalf("default = %q", got)
	}
	if got := (Guardrail{Name: "x"}).resolvedName(); got != "x" {
		t.Fatalf("explicit = %q", got)
	}
}

func TestGuardrailCovers(t *testing.T) {
	g := Guardrail{Stages: []GuardrailStage{StageInput, StageToolOutput}}
	if !g.Covers(StageInput) || !g.Covers(StageToolOutput) {
		t.Error("declared stages should be covered")
	}
	if g.Covers(StageOutput) || g.Covers(StageToolInput) {
		t.Error("undeclared stages must not be covered")
	}
	// A guardrail with no stages is never consulted.
	if (Guardrail{}).Covers(StageInput) {
		t.Error("a guardrail with no stages covers nothing")
	}
}

func TestSelectStage(t *testing.T) {
	in := tripping("in", StageInput)
	both := Guardrail{Name: "both", Stages: []GuardrailStage{StageInput, StageOutput}}
	out := tripping("out", StageOutput)

	got := selectStage([]Guardrail{in, both, out}, StageInput)
	if len(got) != 2 || got[0].Name != "in" || got[1].Name != "both" {
		t.Fatalf("input stage selection = %v", names(got))
	}
	got = selectStage([]Guardrail{in, both, out}, StageOutput)
	if len(got) != 2 || got[0].Name != "both" || got[1].Name != "out" {
		t.Fatalf("output stage selection = %v", names(got))
	}
	if got := selectStage([]Guardrail{in, out}, StageToolInput); len(got) != 0 {
		t.Fatalf("tool-input selection = %v, want empty", names(got))
	}
}

func names(gs []Guardrail) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.Name
	}
	return out
}
