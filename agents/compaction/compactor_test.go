package compaction

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// keepEverything excludes nothing, which makes what the index is holding
// directly observable through IncludedEntries.
type keepEverything struct{}

func (keepEverything) Compact(context.Context, *Index) (bool, error) { return false, nil }

// A Compactor may be configured once and reused. Entry ids are unique within a
// SESSION — FileSession numbers them e1, e2 with no session prefix — so an
// index that resumed on a matching id alone would hand one conversation's
// history to another.
func TestCompactorReusedAcrossSessionsKeepsThemApart(t *testing.T) {
	c := New(keepEverything{}, nil)
	ctx := context.Background()

	withID := func(id, text string) agents.SessionEntry {
		e := user(t, text)
		e.ID = id
		return e
	}

	sessionA := []agents.SessionEntry{withID("e1", "a-one"), withID("e2", "a-two")}
	if _, err := c.Compact(ctx, sessionA); err != nil {
		t.Fatal(err)
	}

	sessionB := []agents.SessionEntry{withID("e1", "b-one"), withID("e2", "b-two"), withID("e3", "b-three")}
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
	idx := NewIndex([]agents.SessionEntry{user(t, "one"), user(t, "two")}, nil)
	before := len(idx.Groups)

	idx.Update([]agents.SessionEntry{user(t, "one"), user(t, "two"), user(t, "three")})
	if len(idx.Groups) <= before {
		t.Fatalf("groups went from %d to %d — the third entry was not appended", before, len(idx.Groups))
	}
	if got := len(idx.indexed); got != 3 {
		t.Fatalf("indexed %d entries, want 3", got)
	}
}
