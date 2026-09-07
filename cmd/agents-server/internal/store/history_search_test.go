package store

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

func assistantEntry(t *testing.T, text string) session.Entry {
	t.Helper()
	return rawEntryFrom(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+quoteJSON(text)+`}],"status":"completed"}`,
		agents.Source{Type: agents.SourceModel})
}

func hitTexts(hits []session.Entry) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = session.RenderItem(h.Item)
	}
	return out
}

// Compacted rows are exactly what the search exists for: Entries leaves them
// out of the model's view, SearchHistory reads them back.
func TestSearchHistoryReadsCompactedRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewEntryStoreFor(db, session.Direct(NewID()))
	seed(t, sa, userEntry(t, "Who wrote the parser?"), assistantEntry(t, "Ada wrote the PARSER."), userEntry(t, "thanks"))
	entries, err := sa.load(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	markCompacted(t, sa, entries[0].ID, entries[1].ID)

	visible, _ := sa.Entries(ctx, session.Cursor{})
	if len(visible) != 1 {
		t.Fatalf("Entries must leave compacted rows out, got %d", len(visible))
	}
	hits, more, err := sa.SearchHistory(ctx, session.HistoryQuery{Query: "parser"})
	if err != nil {
		t.Fatal(err)
	}
	if more || len(hits) != 2 || hits[0].ID != entries[1].ID || hits[1].ID != entries[0].ID {
		t.Fatalf("hits = %v more=%v", hitTexts(hits), more)
	}
	if got, err := sa.Entry(ctx, entries[0].ID); err != nil || got == nil {
		t.Fatalf("a compacted row reads by id: %v %v", got, err)
	}
}

// An abandoned branch is not the conversation: its rows never match.
func TestSearchHistoryStaysOnTheActiveBranch(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewEntryStoreFor(db, session.Direct(NewID()))
	seed(t, sa, userEntry(t, "question"), assistantEntry(t, "abandoned answer"))
	entries, _ := sa.load(ctx, false)
	if err := session.NewSession(sa).Branch(ctx, entries[0].ID); err != nil {
		t.Fatal(err)
	}
	seed(t, sa, assistantEntry(t, "kept answer"))

	hits, _, err := sa.SearchHistory(ctx, session.HistoryQuery{Query: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || session.RenderItem(hits[0].Item) != "kept answer" {
		t.Fatalf("hits = %v", hitTexts(hits))
	}
}

// The SQL narrowing is case-folded and wildcard-safe, and a query the JSON
// encoding would alter falls back to a full read that still matches.
func TestSearchHistoryQueryShapes(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewEntryStoreFor(db, session.Direct(NewID()))
	seed(t, sa,
		userEntry(t, "progress: 100% done"),
		userEntry(t, "he said \"hi\" <b>"),
		userEntry(t, "Ünïcode Straße"),
		userEntry(t, "a1b"),
	)
	cases := []struct {
		query string
		want  int
	}{
		{"100%", 1}, {"1%0", 0}, {"DONE", 1},
		{`"hi" <b>`, 1}, {"straße", 1}, {"a_b", 0}, {"a1b", 1},
	}
	for _, c := range cases {
		hits, _, err := sa.SearchHistory(ctx, session.HistoryQuery{Query: c.query})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != c.want {
			t.Errorf("query %q: %d hits (%v), want %d", c.query, len(hits), hitTexts(hits), c.want)
		}
	}
}

func TestSearchHistoryPagesAndFiltersTools(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewEntryStoreFor(db, session.Direct(NewID()))
	seed(t, sa,
		userEntry(t, "one"),
		rawEntryFrom(t, `{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}`, agents.Source{Type: agents.SourceModel}),
		toolOutputEntry(t, "c1", "main.go"),
		rawEntryFrom(t, `{"type":"function_call","call_id":"c2","name":"read_file","arguments":"{}"}`, agents.Source{Type: agents.SourceModel}),
		toolOutputEntry(t, "c2", "package main"),
		userEntry(t, "two"),
		userEntry(t, "three"),
	)
	first, more, err := sa.SearchHistory(ctx, session.HistoryQuery{Role: "user", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !more || strings.Join(hitTexts(first), ",") != "three,two" {
		t.Fatalf("first page = %v more=%v", hitTexts(first), more)
	}
	second, more, err := sa.SearchHistory(ctx, session.HistoryQuery{Role: "user", Limit: 2, Before: first[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if more || strings.Join(hitTexts(second), ",") != "one" {
		t.Fatalf("second page = %v more=%v", hitTexts(second), more)
	}
	tools, _, err := sa.SearchHistory(ctx, session.HistoryQuery{ToolName: "exec_command"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(hitTexts(tools), ",") != `main.go,exec_command({"cmd":"ls"})` {
		t.Fatalf("tool filter = %v", hitTexts(tools))
	}
	if none, _, _ := sa.SearchHistory(ctx, session.HistoryQuery{Before: "nope"}); len(none) != 0 {
		t.Fatal("an unknown before id matches nothing")
	}
}
