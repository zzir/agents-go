package agents_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// One paused turn, two calls to the same tool: "always approve" on the first
// and "reject" on the second. The second must not run.
func TestOneOffRejectOutranksAlwaysApprove(t *testing.T) {
	ctx := context.Background()
	model := agentstest.NewResponseBuilder().
		FunctionCall("delete_db", "call-1", `{"name":"staging"}`).
		FunctionCall("delete_db", "call-2", `{"name":"prod"}`).
		NewTurn().
		Text("done").
		Build()
	var ran atomic.Int64
	var dropped atomic.Value
	type args struct {
		Name string `json:"name"`
	}
	gated := agents.NewTool("delete_db", "drops a database",
		func(_ context.Context, _ *agents.ToolContext, a args) (string, error) {
			ran.Add(1)
			dropped.Store(a.Name)
			return "dropped " + a.Name, nil
		})
	gated.NeedsApproval = true
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{gated}}

	res, err := agents.RunSync(ctx, agent, "clean up", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 2 {
		t.Fatalf("interruptions = %d, want both calls", len(res.Interruptions))
	}
	for _, it := range res.Interruptions {
		if it.CallID == "call-1" {
			res.State.Approve(it, true)
		} else {
			res.State.Reject(it, false, "not prod")
		}
	}
	res, err = agents.ResumeRunSync(ctx, res.State, agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "done")
	if ran.Load() != 1 || dropped.Load() != "staging" {
		t.Fatalf("the tool ran %d time(s), last on %v; want once, on staging", ran.Load(), dropped.Load())
	}
	var outputs []string
	for _, item := range model.LastRequest().Input {
		if item.OfFunctionCallOutput == nil {
			continue
		}
		raw, err := session.MarshalInputItem(item)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, string(raw))
	}
	if joined := strings.Join(outputs, "|"); !strings.Contains(joined, "dropped staging") || !strings.Contains(joined, "not prod") {
		t.Errorf("tool outputs the model saw = %s, want the first call's result and the second's rejection", joined)
	}
}
