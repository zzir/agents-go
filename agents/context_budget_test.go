package agents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// budgetScript is a tool-call turn and a text turn, each with a usage the
// notice can be checked against.
func budgetScript() *agentstest.FakeModel {
	return agentstest.NewResponseBuilder().
		FunctionCall("get_time", "call-1", "{}").
		Usage(agents.RequestUsage{InputTokens: 1000, OutputTokens: 50, TotalTokens: 1050}).
		NewTurn().
		Text("noon").
		Usage(agents.RequestUsage{InputTokens: 1200, OutputTokens: 10, TotalTokens: 1210}).
		Build()
}

// lastInput returns the role and text of a request's last input item.
func lastInput(t *testing.T, req agents.ModelRequest) (role, text string) {
	t.Helper()
	if len(req.Input) == 0 {
		t.Fatal("request carried no input")
	}
	item := req.Input[len(req.Input)-1]
	raw, err := session.MarshalInputItem(item)
	if err != nil {
		t.Fatal(err)
	}
	return session.ProbeItem(raw).Role, session.ItemText(item)
}

// The notice is the last input item of every call: the host's figure before
// the run's first call, the run's own last call after that. The instructions
// and the session never see it (spec §2.5i).
func TestContextBudgetNoticeIsTheLastInputItem(t *testing.T) {
	ctx := context.Background()
	model := budgetScript()
	agent := &agents.Agent{
		Name:         "a",
		ModelImpl:    model,
		Instructions: agents.StaticInstructions("be brief"),
		Tools:        []*agents.Tool{timeTool(t)},
	}
	sess := session.NewInMemorySession()
	_, err := agents.RunSync(ctx, agent, "time?", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
		Model:        agents.ModelOptions{InputFilter: agents.ContextBudget{Window: 10_000, Occupied: 4_000}.InputFilter()},
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	if role, text := lastInput(t, reqs[0]); role != "system" || text != "Context budget: about 4000 of 10000 tokens in use (60% left)." {
		t.Fatalf("first call: role=%q text=%q", role, text)
	}
	if role, text := lastInput(t, reqs[1]); role != "system" || text != "Context budget: about 1050 of 10000 tokens in use (90% left)." {
		t.Fatalf("second call: role=%q text=%q", role, text)
	}
	for i, req := range reqs {
		if req.SystemInstructions != "be brief" {
			t.Fatalf("call %d: instructions changed to %q", i, req.SystemInstructions)
		}
	}
	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(string(e.Item), "Context budget") {
			t.Fatalf("the notice was persisted: %s", e.Item)
		}
	}
}

// No window, or no figure yet, sends nothing: the user's message stays last.
func TestContextBudgetSendsNothingWithoutAFigure(t *testing.T) {
	ctx := context.Background()
	run := func(b agents.ContextBudget) []agents.ModelRequest {
		model := budgetScript()
		agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{timeTool(t)}}
		if _, err := agents.RunSync(ctx, agent, "time?", agents.RunOptions{
			Model: agents.ModelOptions{InputFilter: b.InputFilter()},
		}); err != nil {
			t.Fatal(err)
		}
		return model.Requests()
	}

	for i, req := range run(agents.ContextBudget{Occupied: 4_000}) {
		if role, _ := lastInput(t, req); role == "system" {
			t.Fatalf("call %d: a notice without a window", i)
		}
	}

	reqs := run(agents.ContextBudget{Window: 10_000})
	if role, _ := lastInput(t, reqs[0]); role == "system" {
		t.Fatal("first call: a notice with no figure to report")
	}
	if role, text := lastInput(t, reqs[1]); role != "system" || !strings.HasPrefix(text, "Context budget: about 1050 of 10000") {
		t.Fatalf("second call: role=%q text=%q", role, text)
	}
}
