package agents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agentstest"
)

// inputShape renders a model request's input for assertions: calls and outputs
// by name/id, everything else by its text.
func inputShape(items []agents.InputItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		switch {
		case it.OfFunctionCall != nil:
			parts = append(parts, "call:"+it.OfFunctionCall.Name)
		case it.OfFunctionCallOutput != nil:
			parts = append(parts, "out:"+it.OfFunctionCallOutput.CallID)
		default:
			parts = append(parts, session.ItemText(it))
		}
	}
	return strings.Join(parts, "|")
}

// The run keeps one item log: RunState.GeneratedItems is the tail of
// SessionItems the model still sees after a handoff input filter restarted the
// view, and a resume — through JSON, in another process — takes it as such
// (spec §2.1).
func TestRunState_GeneratedItemsAreTheLogsTail(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("transfer_to_target", "c1", `{}`).
		NewTurn().
		FunctionCall("gated", "c2", `{}`).
		NewTurn().
		Text("done").
		Build()
	gated := agents.NewTool("gated", "needs approval",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) { return "ok", nil })
	gated.NeedsApproval = true
	target := &agents.Agent{Name: "target", ModelImpl: model, Tools: []*agents.Tool{gated}}
	agent := &agents.Agent{Name: "a", ModelImpl: model, Handoffs: []agents.Handoff{agents.HandoffTo(target)}}
	opts := agents.RunOptions{Exec: agents.ExecOptions{
		HandoffInputFilter: func(agents.HandoffInputData) agents.HandoffInputData {
			return agents.HandoffInputData{InputHistory: agents.InputItemsFromText("start over")}
		},
	}}

	res, err := agents.RunSync(t.Context(), agent, "go", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("interruptions = %d, want 1", len(res.Interruptions))
	}
	st := res.State
	// The log holds the handoff (call and output) and then the gated call; the
	// filter restarted the model's view between the two.
	if st.SessionItems[0].Kind != agents.ItemHandoffCall {
		t.Fatalf("log[0] = %s, want the handoff call", st.SessionItems[0].Kind)
	}
	tail := len(st.SessionItems) - len(st.GeneratedItems)
	if tail == 0 || st.GeneratedItems[0].Kind != agents.ItemToolCall {
		t.Fatalf("generated = %d of %d items, first %s; want the gated call only",
			len(st.GeneratedItems), len(st.SessionItems), st.GeneratedItems[0].Kind)
	}
	for i, it := range st.GeneratedItems {
		if st.SessionItems[tail+i] != it {
			t.Fatalf("GeneratedItems[%d] is not SessionItems[%d]", i, tail+i)
		}
	}

	data, err := st.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := agents.RunStateFromJSON(data, map[string]*agents.Agent{"a": agent, "target": target})
	if err != nil {
		t.Fatal(err)
	}
	restored.Approve(restored.Interruptions[0], false)
	if _, err := agents.ResumeRunSync(t.Context(), restored, opts); err != nil {
		t.Fatal(err)
	}
	agentstest.AssertScriptExhausted(t, model)
	// The resumed run's next call sends the filtered input plus the tail — the
	// handoff the filter dropped stays in the log and reaches no request.
	if got, want := inputShape(model.LastRequest().Input), "start over|call:gated|out:c2"; got != want {
		t.Errorf("resumed model input = %q, want %q", got, want)
	}
}
