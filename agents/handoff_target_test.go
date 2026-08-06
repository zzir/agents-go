package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
)

// A handoff declared as pure data — Target set, no OnInvoke — switches the run.
func TestHandoffTargetAloneSwitches(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("transfer_to_billing", "call-1", "{}").
		NewTurn().
		Text("handled").
		Build()
	billing := &agents.Agent{Name: "billing", ModelImpl: model}
	root := &agents.Agent{
		Name:      "root",
		ModelImpl: model,
		Handoffs: []agents.Handoff{{
			ToolName: "transfer_to_billing",
			Target:   billing,
		}},
	}

	res, err := agents.RunSync(context.Background(), root, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "handled")
	if res.LastAgent != billing {
		t.Errorf("LastAgent = %v, want billing", res.LastAgent)
	}
}

// OnInvoke is the runtime authority: when both are set, its choice wins over
// the static Target declaration.
func TestHandoffOnInvokeOverridesTarget(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("transfer_to_support", "call-1", "{}").
		NewTurn().
		Text("dynamic").
		Build()
	static := &agents.Agent{Name: "static", ModelImpl: model}
	dynamic := &agents.Agent{Name: "dynamic", ModelImpl: model}
	root := &agents.Agent{
		Name:      "root",
		ModelImpl: model,
		Handoffs: []agents.Handoff{{
			ToolName: "transfer_to_support",
			Target:   static,
			OnInvoke: func(context.Context, *agents.RunContext, string) (*agents.Agent, error) {
				return dynamic, nil
			},
		}},
	}

	res, err := agents.RunSync(context.Background(), root, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.LastAgent != dynamic {
		t.Errorf("LastAgent = %v, want dynamic (OnInvoke's choice)", res.LastAgent)
	}
}

// A handoff with neither Target nor OnInvoke cannot deliver an agent to switch
// to; selecting it fails the run with a UserError.
func TestHandoffNeitherTargetNorOnInvoke(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("transfer_to_nowhere", "call-1", "{}").
		Build()
	root := &agents.Agent{
		Name:      "root",
		ModelImpl: model,
		Handoffs:  []agents.Handoff{{ToolName: "transfer_to_nowhere"}},
	}

	_, err := agents.RunSync(context.Background(), root, "hi", agents.RunOptions{})
	var ue *agents.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *agents.UserError", err)
	}
}

// HandoffTo declares its target statically: plain data, no closure.
func TestHandoffToFillsTarget(t *testing.T) {
	billing := &agents.Agent{Name: "billing"}
	h := agents.HandoffTo(billing)
	if h.Target != billing {
		t.Errorf("Target = %v, want billing", h.Target)
	}
	if h.OnInvoke != nil {
		t.Error("OnInvoke != nil; HandoffTo should declare the target as data, not wrap it in a closure")
	}
}
