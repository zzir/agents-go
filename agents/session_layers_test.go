package agents

import (
	"context"
	"testing"
)

// A cursor pages on sequence numbers rather than offsets, so a concurrent
// append cannot make page two skip or repeat an entry.
func TestCursor_PagesOnSequenceNotOffset(t *testing.T) {
	ctx := context.Background()
	st := NewInMemoryStorage("s")
	sess := NewSession(st)
	for _, text := range []string{"a", "b", "c"} {
		if err := sess.AppendItems(ctx, InputItemsFromText(text), Source{}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := sess.Entries(ctx, Cursor{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("page 1 = %d entries, want 2", len(first))
	}

	// Something lands between the two reads — which is exactly what an offset
	// cannot survive.
	if err := sess.AppendItems(ctx, InputItemsFromText("d"), Source{}); err != nil {
		t.Fatal(err)
	}

	next, err := sess.Entries(ctx, Cursor{AfterSeq: first[len(first)-1].Seq})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 2 {
		t.Fatalf("page 2 = %d entries, want the 2 that follow", len(next))
	}
	for _, e := range next {
		if e.Seq <= first[len(first)-1].Seq {
			t.Errorf("page 2 repeated seq %d", e.Seq)
		}
	}
}

// A negative limit takes the most recent N, which is how a run bounds the
// history it loads.
func TestCursor_NegativeLimitTakesTheTail(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()
	for _, text := range []string{"a", "b", "c", "d"} {
		if err := sess.AppendItems(ctx, InputItemsFromText(text), Source{}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := sess.Entries(ctx, Cursor{Limit: -2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want the last 2", len(got))
	}
	// By identity, not by number: a sequence number is a cursor position, not
	// a count, so a test that spells one out is testing the allocator.
	if got[0].ID != all[2].ID || got[1].ID != all[3].ID {
		t.Errorf("got %s,%s, want the last two (%s,%s) — still oldest-first",
			got[0].ID, got[1].ID, all[2].ID, all[3].ID)
	}
}

// A compaction checkpoint reshapes the model's view: what it folded is dropped
// wherever it sits, what was kept stays as ordinary entries, and the summary
// renders up front. Re-sending the folded history would undo the compaction —
// and the kept history comes from the session itself, never from a copy inside
// the checkpoint.
func TestContextEntries_DropFoldedHistory(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()
	if err := sess.AppendItems(ctx, InputItemsFromText("ancient"), Source{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(ctx, InputItemsFromText("kept"), Source{}); err != nil {
		t.Fatal(err)
	}
	all, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := NewCompactionEntry(CompactionPayload{Summary: "SUMMARY", ExcludedIDs: []string{all[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendItems(ctx, InputItemsFromText("recent"), Source{}); err != nil {
		t.Fatal(err)
	}

	entries, err := sess.ContextEntries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	// kept + checkpoint + recent; the folded entry is out of the view.
	if len(entries) != 3 || entries[1].Kind != EntryKindCompaction {
		t.Fatalf("context entries = %d, want the kept entry, the checkpoint and what followed", len(entries))
	}
	for _, e := range entries {
		if e.ID == all[0].ID {
			t.Error("the folded entry is still in the context view")
		}
	}

	items, err := sess.ContextItems(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	// summary first, then the kept and recent entries in order.
	if len(items) != 3 {
		t.Fatalf("context items = %d, want 3", len(items))
	}
	if raw, _ := MarshalInputItem(items[0]); !contains(raw, "SUMMARY") {
		t.Errorf("the summary does not render first: %s", raw)
	}
	for _, item := range items {
		raw, _ := MarshalInputItem(item)
		if string(raw) != "" && contains(raw, "ancient") {
			t.Error("history folded away by compaction was re-sent")
		}
	}
}

// State is a fold over the log, not a field kept beside it: given the same
// entries it gives the same answer, with no cache to invalidate.
func TestReduceState(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()

	callItem, err := UnmarshalInputItem([]byte(`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`))
	if err != nil {
		t.Fatal(err)
	}
	paired, err := UnmarshalInputItem([]byte(`{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"}`))
	if err != nil {
		t.Fatal(err)
	}
	output, err := UnmarshalInputItem([]byte(`{"type":"function_call_output","call_id":"c2","output":"done"}`))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := NewItemEntries([]InputItem{callItem, paired, output}, Source{})
	if err != nil {
		t.Fatal(err)
	}
	entries[0].AgentName = "first"
	entries[2].AgentName = "second"
	entries[2].ResponseID = "resp_9"
	entries[2].Usage = &RequestUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	if err := sess.Append(ctx, entries...); err != nil {
		t.Fatal(err)
	}

	st, err := sess.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAgent != "second" {
		t.Errorf("LastAgent = %q, want the most recent", st.LastAgent)
	}
	if st.LastResponseID != "resp_9" {
		t.Errorf("LastResponseID = %q", st.LastResponseID)
	}
	// c1 never got its output; c2 did.
	if len(st.PendingCallIDs) != 1 || st.PendingCallIDs[0] != "c1" {
		t.Errorf("PendingCallIDs = %v, want [c1]", st.PendingCallIDs)
	}
	if st.Usage.TotalTokens != 15 || st.Requests != 1 {
		t.Errorf("usage = %+v, requests = %d", st.Usage, st.Requests)
	}

	stats, err := sess.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entries != 3 || stats.Items != 3 {
		t.Errorf("stats = %+v", stats)
	}
}

// Popping is not a storage primitive — a run never pops — so a store that
// cannot do it says so instead of every backend implementing it.
func TestPopEntry_RequiresACapableStore(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()
	if err := sess.AppendItems(ctx, InputItemsFromText("only"), Source{}); err != nil {
		t.Fatal(err)
	}
	got, err := sess.PopEntry(ctx)
	if err != nil || got == nil {
		t.Fatalf("pop on capable storage: %v, %v", got, err)
	}

	incapable := NewSession(readOnlyStorage{})
	if _, err := incapable.PopEntry(ctx); err == nil {
		t.Error("popping a store that cannot pop should report it, not silently do nothing")
	}
}

type readOnlyStorage struct{}

func (readOnlyStorage) Metadata(context.Context) (SessionMetadata, error) {
	return SessionMetadata{}, nil
}
func (readOnlyStorage) Append(context.Context, ...SessionEntry) error { return nil }
func (readOnlyStorage) Entry(context.Context, string) (*SessionEntry, error) {
	return nil, nil
}
func (readOnlyStorage) Entries(context.Context, Cursor) ([]SessionEntry, error) {
	return nil, nil
}
func (readOnlyStorage) Clear(context.Context) error { return nil }

func contains(b []byte, sub string) bool {
	return len(b) >= len(sub) && string(b) != "" && indexOf(string(b), sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
