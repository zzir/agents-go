package agents_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
)

// assertExtends fails unless next's input starts with prev's, item for item
// and byte for byte; sameSystem pins the instructions too.
func assertExtends(t *testing.T, prev, next agents.ModelRequest, sameSystem bool) {
	t.Helper()
	if len(prev.Input) > len(next.Input) {
		t.Fatalf("the next request carries %d input items, fewer than the %d before it", len(next.Input), len(prev.Input))
	}
	for i := range prev.Input {
		was, err := json.Marshal(prev.Input[i])
		if err != nil {
			t.Fatal(err)
		}
		now, err := json.Marshal(next.Input[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(was, now) {
			t.Fatalf("input item %d changed between calls:\n was %s\n now %s", i, was, now)
		}
	}
	if sameSystem && prev.SystemInstructions != next.SystemInstructions {
		t.Fatalf("instructions changed between calls: %q, then %q", prev.SystemInstructions, next.SystemInstructions)
	}
}

// assertChain checks every consecutive pair of n requests.
func assertChain(t *testing.T, reqs []agents.ModelRequest, n int, sameSystem bool) {
	t.Helper()
	if len(reqs) != n {
		t.Fatalf("model calls = %d, want %d", len(reqs), n)
	}
	for i := 1; i < len(reqs); i++ {
		assertExtends(t, reqs[i-1], reqs[i], sameSystem)
	}
}

// Each model request extends the one before it: a backend that binds replayed
// reasoning to its prefix rejects, or forgets, anything else.
func TestModelInputAppendOnly(t *testing.T) {
	ctx := context.Background()

	t.Run("tool_loop_with_reasoning", func(t *testing.T) {
		model := agentstest.NewResponseBuilder().
			Reasoning("which clock").FunctionCall("get_time", "call-1", "{}").
			NewTurn().
			Reasoning("check again").FunctionCall("get_time", "call-2", "{}").
			NewTurn().
			Text("noon").
			Build()
		agent := &agents.Agent{
			Name:         "a",
			ModelImpl:    model,
			Instructions: agents.StaticInstructions("be brief"),
			Tools:        []*agents.Tool{timeTool(t)},
		}
		if _, err := agents.RunSync(ctx, agent, "time?", agents.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		assertChain(t, model.Requests(), 3, true)
	})

	t.Run("steer_at_save_point", func(t *testing.T) {
		model := agentstest.NewResponseBuilder().
			FunctionCall("probe", "call-1", "{}").
			NewTurn().
			Text("done").
			Build()
		var ctrl agents.RunControl
		probe := agents.NewTool("probe", "probes",
			func(context.Context, *agents.ToolContext, struct{}) (string, error) {
				return "ok", ctrl.Steer("and the date")
			})
		agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{probe}}
		stream, c := agents.Run(ctx, agent, "go", agents.RunOptions{})
		ctrl = c
		if _, err := stream.Collect(); err != nil {
			t.Fatal(err)
		}
		assertChain(t, model.Requests(), 2, true)
	})

	t.Run("follow_up", func(t *testing.T) {
		model := agentstest.TextModel("first", "second")
		agent := &agents.Agent{Name: "a", ModelImpl: model}
		stream, ctrl := agents.Run(ctx, agent, "go", agents.RunOptions{})
		if err := ctrl.FollowUp("and tomorrow?"); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Collect(); err != nil {
			t.Fatal(err)
		}
		assertChain(t, model.Requests(), 2, true)
	})

	t.Run("approval_resume", func(t *testing.T) {
		model := agentstest.NewResponseBuilder().
			FunctionCall("delete_db", "call-1", "{}").
			NewTurn().
			Text("gone").
			Build()
		gated := agents.NewTool("delete_db", "drops it",
			func(context.Context, *agents.ToolContext, struct{}) (string, error) {
				return "dropped", nil
			})
		gated.NeedsApproval = true
		agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{gated}}
		res, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Interruptions) != 1 {
			t.Fatalf("interruptions = %d, want the gated call", len(res.Interruptions))
		}
		res.State.Approve(res.Interruptions[0], false)
		if _, err := agents.ResumeRunSync(ctx, res.State, agents.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		assertChain(t, model.Requests(), 2, true)
	})

	t.Run("handoff", func(t *testing.T) {
		model := agentstest.NewResponseBuilder().
			FunctionCall("transfer_to_billing", "call-1", "{}").
			NewTurn().
			Text("handled").
			Build()
		billing := &agents.Agent{Name: "billing", ModelImpl: model, Instructions: agents.StaticInstructions("bill")}
		root := &agents.Agent{
			Name:         "root",
			ModelImpl:    model,
			Instructions: agents.StaticInstructions("route"),
			Handoffs:     []agents.Handoff{{ToolName: "transfer_to_billing", Target: billing}},
		}
		if _, err := agents.RunSync(ctx, root, "hi", agents.RunOptions{}); err != nil {
			t.Fatal(err)
		}
		// The instructions change by design: a handoff is a change of agent
		// (spec §2.4).
		assertChain(t, model.Requests(), 2, false)
	})

	t.Run("context_budget", func(t *testing.T) {
		t.Skip("known breakpoint: the notice is removed on the next call — spec §2.5i")
		model := budgetScript()
		agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{timeTool(t)}}
		_, err := agents.RunSync(ctx, agent, "time?", agents.RunOptions{
			Model: agents.ModelOptions{InputFilter: agents.ContextBudget{Window: 10_000, Occupied: 4_000}.InputFilter()},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertChain(t, model.Requests(), 2, true)
	})
}
