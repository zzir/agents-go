package agents_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
)

// A nil control to ResumeRunWith is ResumeRun's own: a fresh one seeded from
// the paused state's queue (spec §2.11b).
func TestResumeRunWithNilControlSeedsFromState(t *testing.T) {
	gated := agents.NewTool("gated", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "done", nil
	})
	gated.NeedsApproval = true
	model := agentstest.NewResponseBuilder().
		FunctionCall("gated", "call-1", "{}").
		NewTurn().
		Text("first answer").
		NewTurn().
		Text("second answer").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{gated}}

	stream, ctrl := agents.Run(context.Background(), agent, "go", agents.RunOptions{})
	if err := ctrl.FollowUp("and then?"); err != nil {
		t.Fatal(err)
	}
	paused, err := stream.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want the gated call", len(paused.Interruptions))
	}
	paused.State.Approve(paused.Interruptions[0], false)

	res, err := agents.ResumeRunWith(context.Background(), paused.State, agents.RunOptions{}, nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	// The follow-up rode the state into the resumed run and extended it.
	agentstest.AssertFinalOutput(t, res, "second answer")
	agentstest.AssertScriptExhausted(t, model)
}
