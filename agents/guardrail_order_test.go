package agents_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// replacePair declares two guardrails at stage that both Replace: "second"
// finishes at once, "first" only after second returned and a head start
// passed, so completion order is the reverse of declaration order.
func replacePair(stage agents.GuardrailStage) []agents.Guardrail {
	secondDone := make(chan struct{})
	run := func(name string) func(context.Context, *agents.RunContext, agents.GuardrailPayload) (agents.GuardrailDecision, error) {
		return func(ctx context.Context, _ *agents.RunContext, _ agents.GuardrailPayload) (agents.GuardrailDecision, error) {
			if name == "second" {
				defer close(secondDone)
				return agents.Replace("from second", nil), nil
			}
			select {
			case <-secondDone:
			case <-ctx.Done():
				return agents.GuardrailDecision{}, ctx.Err()
			}
			time.Sleep(20 * time.Millisecond)
			return agents.Replace("from first", nil), nil
		}
	}
	return []agents.Guardrail{
		{Name: "first", Stages: []agents.GuardrailStage{stage}, Blocking: true, Run: run("first")},
		{Name: "second", Stages: []agents.GuardrailStage{stage}, Blocking: true, Run: run("second")},
	}
}

func resultNames(results []agents.GuardrailResult) string {
	names := make([]string, 0, len(results))
	for _, r := range results {
		names = append(names, r.Guardrail.Name)
	}
	return strings.Join(names, ",")
}

// Two concurrent output guardrails both Replace and the later-declared one
// finishes first: the first declared still wins, and the results are reported
// in declaration order (spec §2.6).
func TestConcurrentOutputGuardrailsReplaceInDeclarationOrder(t *testing.T) {
	agent := &agents.Agent{
		Name:       "a",
		ModelImpl:  agentstest.TextModel("original"),
		Guardrails: replacePair(agents.StageOutput),
	}
	res, err := agents.RunSync(t.Context(), agent, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.FinalOutputString(); got != "from first" {
		t.Errorf("final output = %q, want the first declared guardrail's replacement", got)
	}
	if got := resultNames(res.GuardrailResults); got != "first,second" {
		t.Errorf("results = %s, want declaration order first,second", got)
	}
}

// The same at the input stage: two blocking input guardrails both Replace,
// the later one finishes first, and the model is called with the first
// declared replacement.
func TestConcurrentInputGuardrailsReplaceInDeclarationOrder(t *testing.T) {
	model := agentstest.TextModel("ok")
	agent := &agents.Agent{
		Name:       "a",
		ModelImpl:  model,
		Guardrails: replacePair(agents.StageInput),
	}
	res, err := agents.RunSync(t.Context(), agent, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, it := range model.LastRequest().Input {
		sent = append(sent, session.ItemText(it))
	}
	if got := strings.Join(sent, "|"); got != "from first" {
		t.Errorf("model input = %q, want the first declared guardrail's replacement", got)
	}
	if got := resultNames(res.GuardrailResults); got != "first,second" {
		t.Errorf("results = %s, want declaration order first,second", got)
	}
}
