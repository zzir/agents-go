package agents

import (
	"context"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

func fnCall(callID string) InputItem {
	return responses.ResponseInputItemParamOfFunctionCall("{}", callID, "tool")
}

func fnOutput(callID, output string) InputItem {
	return responses.ResponseInputItemParamOfFunctionCallOutput(callID, output)
}

func reasoningItem(id string) InputItem {
	return responses.ResponseInputItemParamOfReasoning(id, nil)
}

// callIDsOf returns the call ids of every function_call item, in order.
func callIDsOf(items []InputItem) []string {
	var out []string
	for _, it := range items {
		if it.OfFunctionCall != nil {
			out = append(out, it.OfFunctionCall.CallID)
		}
	}
	return out
}

func TestNormalizeStoredInput_DropsOrphanCall(t *testing.T) {
	items := []InputItem{
		userMsg("hi"),
		fnCall("c1"),
		fnOutput("c1", "done"), // c1 is paired — keep
		fnCall("c2"),
		// c2 has no output — orphan, drop
		userMsg("bye"),
	}
	got := normalizeStoredInput(items)
	ids := callIDsOf(got)
	if len(ids) != 1 || ids[0] != "c1" {
		t.Errorf("kept call ids = %v, want [c1] (orphan c2 dropped)", ids)
	}
	if len(got) != 4 {
		t.Errorf("len = %d, want 4 (two messages + c1 call + its output)", len(got))
	}
}

func TestNormalizeStoredInput_DropsReasoningBeforeOrphan(t *testing.T) {
	items := []InputItem{
		userMsg("hi"),
		reasoningItem("rs_kept"),
		fnCall("paired"),
		fnOutput("paired", "done"), // paired: reasoning rs_kept stays
		reasoningItem("rs_orphan"),
		fnCall("c2"), // orphan: rs_orphan must be dropped with it
	}
	got := normalizeStoredInput(items)
	var reasoningIDs []string
	for _, it := range got {
		if it.OfReasoning != nil {
			reasoningIDs = append(reasoningIDs, it.OfReasoning.ID)
		}
	}
	if len(reasoningIDs) != 1 || reasoningIDs[0] != "rs_kept" {
		t.Errorf("reasoning ids = %v, want [rs_kept] (rs_orphan dropped with its call)", reasoningIDs)
	}
	if ids := callIDsOf(got); len(ids) != 1 || ids[0] != "paired" {
		t.Errorf("call ids = %v, want [paired]", ids)
	}
}

func TestNormalizeStoredInput_DedupesPreferringLatest(t *testing.T) {
	// The same completed call appears twice (session history + a re-sent copy).
	items := []InputItem{
		fnCall("c1"),
		fnOutput("c1", "first"),
		fnCall("c1"),             // duplicate call_id
		fnOutput("c1", "second"), // duplicate call_id — latest output wins
	}
	got := normalizeStoredInput(items)
	if ids := callIDsOf(got); len(ids) != 1 {
		t.Errorf("call ids = %v, want one c1 after dedupe", ids)
	}
	// The surviving output is the latest one.
	var lastOutput string
	for _, it := range got {
		if it.OfFunctionCallOutput != nil {
			lastOutput = it.OfFunctionCallOutput.Output.OfString.Value
		}
	}
	if lastOutput != "second" {
		t.Errorf("surviving output = %q, want %q (latest wins)", lastOutput, "second")
	}
}

func TestNormalizeStoredInput_KeepsCleanHistory(t *testing.T) {
	items := []InputItem{
		userMsg("hi"),
		fnCall("c1"),
		fnOutput("c1", "done"),
		asstMsg("all set"),
	}
	got := normalizeStoredInput(items)
	if len(got) != len(items) {
		t.Errorf("clean history was modified: len %d → %d", len(items), len(got))
	}
}

func TestNormalizeStoredInput_KeepsDuplicateMessages(t *testing.T) {
	// Messages carry no stable id in easy form, so identical messages must not
	// collapse.
	items := []InputItem{userMsg("hi"), userMsg("hi")}
	if got := normalizeStoredInput(items); len(got) != 2 {
		t.Errorf("duplicate messages collapsed: len = %d, want 2", len(got))
	}
}

// End-to-end: a session holding a dangling function_call (what any client
// persists at an interruption) must not reach the model as an orphan.
func TestRun_SessionOrphanCallScrubbed(t *testing.T) {
	sess := session.NewInMemorySession()
	if err := session.NewSession(sess).AppendItems(context.Background(), []InputItem{
		userMsg("earlier question"),
		fnCall("dangling"), // no output — a turn that paused here
	}, Source{}); err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "answer"))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "new question", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}}); err != nil {
		t.Fatal(err)
	}
	// The model must not have received the dangling call.
	if ids := callIDsOf(model.lastReq.Input); len(ids) != 0 {
		t.Errorf("model saw orphan call ids %v, want none", ids)
	}
}
