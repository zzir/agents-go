package history_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/history"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// seed writes an earlier exchange, then folds it: the model's context no
// longer holds the parser answer, the log still does.
func seed(t *testing.T, sess *session.Session) {
	t.Helper()
	ctx := context.Background()
	if err := sess.AppendItems(ctx, agents.InputItemsFromText("Who wrote the parser?"), agents.Source{Type: agents.SourceUser}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(ctx, agents.InputItemsFromAssistantText("Ada wrote the parser in 2024."), agents.Source{Type: agents.SourceModel}); err != nil {
		t.Fatal(err)
	}
	entries, err := sess.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := session.NewCompactionEntry(session.CompactionPayload{
		Summary:     session.SummaryMarker + " an earlier exchange about the parser",
		ExcludedIDs: []string{entries[0].ID, entries[1].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(ctx, cp); err != nil {
		t.Fatal(err)
	}
}

// toolOutputs is the text every tool returned, in order.
func toolOutputs(res *agents.RunResult) []string {
	var out []string
	for _, it := range res.NewItems {
		if it.Kind == agents.ItemToolCallOutput {
			out = append(out, fmt.Sprint(it.Output))
		}
	}
	return out
}

// The model searches, reads the folded answer back in full, and answers from
// it. The folded entries are not in its context: the request that ran the
// search carries the checkpoint summary and not the parser answer.
func TestHistoryToolsReadWhatCompactionFolded(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	seed(t, sess)

	model := agentstest.NewResponseBuilder().
		FunctionCall("history_search", "c1", `{"query":"parser","role":"","tool_name":"","limit":0,"before":"","scope":""}`).
		NewTurn().
		Text("Ada.").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model,
		Tools: history.Tools(history.For(sess), history.Options{})}
	res, err := agents.RunSync(ctx, agent, "Remind me who wrote the parser.", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatal(err)
	}
	outs := toolOutputs(res)
	if len(outs) != 1 {
		t.Fatalf("tool outputs = %d, want 1", len(outs))
	}
	out := outs[0]
	if !strings.Contains(out, "Ada wrote the parser in 2024.") || !strings.Contains(out, "[id: ") {
		t.Fatalf("search output = %q", out)
	}
	if !strings.Contains(out, "Who wrote the parser?") {
		t.Fatalf("the folded user message is searchable too: %q", out)
	}
	// The question that ran the search is persisted before the model call
	// and so is searchable; the search call itself is not yet.
	if !strings.Contains(out, "Remind me who wrote the parser.") || strings.Contains(out, "history_search(") {
		t.Fatalf("visibility follows persistence: %q", out)
	}
	for _, it := range model.Requests()[0].Input {
		if strings.Contains(session.ItemText(it), "Ada wrote the parser in 2024.") {
			t.Fatal("the folded answer reached the model's context without a read")
		}
	}
}

func TestHistoryReadWindowsOneItem(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	seed(t, sess)
	entries, _ := sess.Entries(ctx, session.Cursor{})
	answer := entries[1].ID

	model := agentstest.NewResponseBuilder().
		FunctionCall("history_read", "c1", `{"id":"`+answer+`","offset":4,"limit":5,"scope":""}`).
		NewTurn().
		FunctionCall("history_read", "c2", `{"id":"nope","offset":0,"limit":0,"scope":""}`).
		NewTurn().
		FunctionCall("history_search", "c3", `{"query":"","role":"","tool_name":"","limit":0,"before":"","scope":"task-1"}`).
		NewTurn().
		Text("done").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model,
		Tools: history.Tools(history.For(sess), history.Options{})}
	res, err := agents.RunSync(ctx, agent, "go", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	})
	if err != nil {
		t.Fatal(err)
	}
	outputs := toolOutputs(res)
	if len(outputs) != 3 {
		t.Fatalf("outputs = %d, want 3", len(outputs))
	}
	if !strings.Contains(outputs[0], "\nwrote") || !strings.Contains(outputs[0], "more characters after offset 9") {
		t.Fatalf("window = %q", outputs[0])
	}
	if !strings.Contains(outputs[1], `No history item has the id "nope"`) {
		t.Fatalf("unknown id = %q", outputs[1])
	}
	if !strings.Contains(outputs[2], `no scope "task-1"`) {
		t.Fatalf("a refused scope reaches the model: %q", outputs[2])
	}
}

func TestHistoryToolsAreReadOnly(t *testing.T) {
	for _, tool := range history.Tools(history.For(session.NewInMemorySession()), history.Options{ScopeHint: "scope: a task id"}) {
		if !tool.ReadOnly {
			t.Fatalf("%s is not read-only", tool.Name)
		}
		if !strings.Contains(tool.Description, "scope: a task id") {
			t.Fatalf("%s description lacks the scope hint", tool.Name)
		}
	}
}
