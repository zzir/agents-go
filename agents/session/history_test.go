package session

import (
	"encoding/json"
	"testing"
)

func historyEntry(id, parent, raw string) Entry {
	return Entry{ID: id, ParentID: parent, Kind: EntryKindItem, Item: json.RawMessage(raw)}
}

// A branch with a folded prefix: the checkpoint hides u1/a1 from the model's
// context, and the search still reads them.
func historyFixture() []Entry {
	cp, _ := NewCompactionEntry(CompactionPayload{Summary: SummaryMarker + " earlier", ExcludedIDs: []string{"u1", "a1"}})
	cp.ID, cp.ParentID = "cp", "o1"
	return []Entry{
		historyEntry("u1", "", `{"role":"user","content":"Ada wrote the parser"}`),
		historyEntry("a1", "u1", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Noted: the PARSER is Ada's"}]}`),
		historyEntry("c1", "a1", `{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{\"path\":\"parser.go\"}"}`),
		historyEntry("o1", "c1", `{"type":"function_call_output","call_id":"call-1","output":"package parser"}`),
		cp,
		historyEntry("r1", "cp", `{"type":"reasoning","summary":[{"type":"summary_text","text":"parser thoughts"}]}`),
		historyEntry("u2", "r1", `{"role":"user","content":"and the lexer?"}`),
	}
}

func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func TestSearchHistoryReadsFoldedEntriesNewestFirst(t *testing.T) {
	hits, more := SearchHistory(historyFixture(), HistoryQuery{Query: "parser"})
	if more {
		t.Fatal("more with everything returned")
	}
	// The reasoning item has no readable text and the checkpoint is no item.
	if got, want := ids(hits), []string{"o1", "c1", "a1", "u1"}; len(got) != len(want) || got[0] != want[0] || got[3] != want[3] {
		t.Fatalf("hits = %v, want %v", got, want)
	}
}

func TestSearchHistoryFilters(t *testing.T) {
	fx := historyFixture()
	if hits, _ := SearchHistory(fx, HistoryQuery{Query: "parser", Role: "user"}); len(hits) != 1 || hits[0].ID != "u1" {
		t.Fatalf("role filter: %v", ids(hits))
	}
	if hits, _ := SearchHistory(fx, HistoryQuery{ToolName: "read_file"}); len(hits) != 2 || hits[0].ID != "o1" || hits[1].ID != "c1" {
		t.Fatalf("tool filter keeps the call and its output: %v", ids(hits))
	}
	if hits, _ := SearchHistory(fx, HistoryQuery{ToolName: "other"}); len(hits) != 0 {
		t.Fatalf("unknown tool: %v", ids(hits))
	}
	if hits, _ := SearchHistory(fx, HistoryQuery{}); len(hits) != 5 {
		t.Fatalf("an empty query lists every readable item: %v", ids(hits))
	}
}

func TestSearchHistoryPagesBackwards(t *testing.T) {
	fx := historyFixture()
	first, more := SearchHistory(fx, HistoryQuery{Limit: 2})
	if !more || len(first) != 2 || first[0].ID != "u2" || first[1].ID != "o1" {
		t.Fatalf("first page = %v more=%v", ids(first), more)
	}
	second, more := SearchHistory(fx, HistoryQuery{Limit: 2, Before: first[1].ID})
	if !more || len(second) != 2 || second[0].ID != "c1" || second[1].ID != "a1" {
		t.Fatalf("second page = %v more=%v", ids(second), more)
	}
	if hits, more := SearchHistory(fx, HistoryQuery{Before: "nope"}); len(hits) != 0 || more {
		t.Fatal("an unknown before id matches nothing")
	}
}

// Off-path entries stay out: the search is a branch-scoped view.
func TestSearchHistoryStaysOnTheActiveBranch(t *testing.T) {
	entries := []Entry{
		historyEntry("u1", "", `{"role":"user","content":"root"}`),
		historyEntry("a1", "u1", `{"type":"message","role":"assistant","content":"abandoned answer"}`),
		historyEntry("a2", "u1", `{"type":"message","role":"assistant","content":"kept answer"}`),
	}
	hits, _ := SearchHistory(entries, HistoryQuery{Query: "answer"})
	if len(hits) != 1 || hits[0].ID != "a2" {
		t.Fatalf("hits = %v, want the active branch only", ids(hits))
	}
}

func TestRenderItemAndRole(t *testing.T) {
	cases := []struct{ raw, role, text string }{
		{`{"role":"user","content":"hi"}`, "user", "hi"},
		{`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}`, "assistant", "ab"},
		{`{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`, "tool", "f({})"},
		{`{"type":"function_call_output","call_id":"c","output":"plain"}`, "tool", "plain"},
		{`{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"x"}]}`, "tool", `[{"type":"input_text","text":"x"}]`},
		{`{"type":"reasoning","summary":[]}`, "", ""},
		{`{"type":"item_reference","id":"x"}`, "", ""},
	}
	for _, c := range cases {
		raw := json.RawMessage(c.raw)
		if got := ItemRole(raw); got != c.role {
			t.Errorf("ItemRole(%s) = %q, want %q", c.raw, got, c.role)
		}
		if got := RenderItem(raw); got != c.text {
			t.Errorf("RenderItem(%s) = %q, want %q", c.raw, got, c.text)
		}
	}
}
