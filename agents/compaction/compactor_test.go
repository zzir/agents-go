package compaction

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// keepEverything excludes nothing, which makes what the index is holding
// directly observable through IncludedEntries.
type keepEverything struct{}

func (keepEverything) Compact(context.Context, *Index) (bool, error) { return false, nil }

// grouped returns every entry the index holds, in the order it grouped them.
// The groups themselves ARE the record of what was indexed, so production code
// reads them directly; only these tests want the flattened view.
func (idx *Index) grouped() []session.Entry {
	out := make([]session.Entry, 0, len(idx.Groups))
	for _, g := range idx.Groups {
		out = append(out, g.Entries...)
	}
	return out
}

// A Compactor may be configured once and reused. Entry ids are unique within a
// SESSION — the filesession store numbers them e1, e2 with no prefix — so an
// index that resumed on a matching id alone would hand one conversation's
// history to another.
func TestCompactorReusedAcrossSessionsKeepsThemApart(t *testing.T) {
	c := New(keepEverything{}, nil)
	ctx := context.Background()

	withID := func(id, text string) session.Entry { return userWithID(t, id, text) }

	sessionA := []session.Entry{withID("e1", "a-one"), withID("e2", "a-two")}
	if _, err := c.Compact(ctx, sessionA); err != nil {
		t.Fatal(err)
	}

	sessionB := []session.Entry{withID("e1", "b-one"), withID("e2", "b-two"), withID("e3", "b-three")}
	got, err := c.Compact(ctx, sessionB)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(sessionB) {
		t.Fatalf("got %d entries, want session B's %d", len(got), len(sessionB))
	}
	for i, e := range got {
		if string(e.Item) != string(sessionB[i].Item) {
			t.Fatalf("entry %d is %s, want session B's %s", i, e.Item, sessionB[i].Item)
		}
	}
}

// The same guard must not cost the incremental path: a genuine continuation of
// the same session still resumes rather than regrouping from scratch.
func TestCompactorResumesOnATrueContinuation(t *testing.T) {
	idx := NewIndex([]session.Entry{user(t, "one"), user(t, "two")}, nil)
	before := len(idx.Groups)

	idx.Update([]session.Entry{user(t, "one"), user(t, "two"), user(t, "three")})
	if len(idx.Groups) <= before {
		t.Fatalf("groups went from %d to %d — the third entry was not appended", before, len(idx.Groups))
	}
	if got := len(idx.grouped()); got != 3 {
		t.Fatalf("indexed %d entries, want 3", got)
	}
}

// Update resumes by comparing against the entries the groups already hold, so
// grouping must keep every entry, in order. If a group ever dropped or
// reordered one, the prefix check would compare the wrong pairs and rebuild the
// whole index on every turn — the exact cost it exists to avoid.
func TestGroupingKeepsEveryEntryInOrder(t *testing.T) {
	entries := withIDs([]session.Entry{
		user(t, "weather?"),
		reasoning(t),
		call(t, "c1", "get_weather"),
		output(t, "c1", "sunny"),
		assistant(t, "sunny"),
		user(t, "and tomorrow?"),
	})
	idx := NewIndex(entries, nil)

	got := idx.grouped()
	if len(got) != len(entries) {
		t.Fatalf("grouped %d entries, want %d", len(got), len(entries))
	}
	for i := range entries {
		if !got[i].Equal(entries[i]) {
			t.Fatalf("entry %d is %s, want %s", i, got[i].Item, entries[i].Item)
		}
	}
}

// Two sessions whose entries hold the same text but different token usage must
// not share an index: ContextTokens reads Usage, so resuming onto the other's
// would compact against a budget that was never measured on this conversation.
func TestUpdateRebuildsWhenOnlyUsageDiffers(t *testing.T) {
	withUsage := func(e session.Entry, in int64) session.Entry {
		e.Usage = &agents.RequestUsage{InputTokens: in, TotalTokens: in}
		return e
	}
	first := []session.Entry{withUsage(userWithID(t, "e1", "hello"), 100)}
	idx := NewIndex(first, nil)
	if got := idx.ContextTokens(); got != 100 {
		t.Fatalf("first session context = %d, want 100", got)
	}

	second := []session.Entry{withUsage(userWithID(t, "e1", "hello"), 9000)}
	idx.Update(second)
	if got := idx.ContextTokens(); got != 9000 {
		t.Fatalf("context = %d, want the second session's 9000 — the index resumed onto a foreign history", got)
	}
}
