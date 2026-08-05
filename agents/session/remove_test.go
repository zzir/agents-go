package session

import "testing"

func mustLeaf(t *testing.T, id, target string) Entry {
	t.Helper()
	e, err := NewLeafEntry(target)
	if err != nil {
		t.Fatal(err)
	}
	e.ID, e.Seq = id, 99
	return e
}

func item(id, parent string, seq int64) Entry {
	return Entry{ID: id, ParentID: parent, Seq: seq, Kind: EntryKindItem}
}

func ann(id, parent string, seq int64) Entry {
	return Entry{ID: id, ParentID: parent, Seq: seq, Kind: EntryKindAnnotation}
}

// PopLast takes the newest entry whatever it is. Nothing can point at it — a
// parent is always older — so it never needs a repair.
func TestPlanPopLastTakesTheNewestEntry(t *testing.T) {
	entries := []Entry{item("a", "", 1), item("b", "a", 2), ann("c", "b", 3)}
	plan, ok := PlanPop(entries, PopLast)
	if !ok || plan.Entry.ID != "c" {
		t.Fatalf("popped %q, want the newest entry c", plan.Entry.ID)
	}
	if len(plan.Relink) != 0 {
		t.Fatalf("popping the newest needed relinks: %v", plan.Relink)
	}
}

// PopLastItem skips what is not an item — and that is exactly when a repair is
// needed, because what it skipped is pointing at what it takes.
func TestPlanPopLastItemRelinksWhatItSkipped(t *testing.T) {
	entries := []Entry{item("a", "", 1), item("b", "a", 2), ann("c", "b", 3)}
	plan, ok := PlanPop(entries, PopLastItem)
	if !ok || plan.Entry.ID != "b" {
		t.Fatalf("popped %q, want the newest item b", plan.Entry.ID)
	}
	if got := plan.Relink["c"]; got != "a" {
		t.Fatalf("the annotation was left pointing at %q, want b's parent a", got)
	}
	// And the result still walks: c's parent exists.
	kept := ApplyRemoval(entries, plan)
	if len(kept) != 2 || kept[1].ParentID != "a" {
		t.Fatalf("after the removal: %+v", kept)
	}
	if len(PathToLeaf(kept, LeafOf(kept))) != 2 {
		t.Fatal("the walk does not reach the root — the removal truncated the session")
	}
}

// A branch pointer at what is going moves to where the branch was before,
// rather than being left aimed at an entry that is not there.
func TestPlanPopRelinksALeafMove(t *testing.T) {
	entries := []Entry{item("a", "", 1), item("b", "a", 2), mustLeaf(t, "l", "b")}
	plan, ok := PlanPop(entries, PopLastItem)
	if !ok || plan.Entry.ID != "b" {
		t.Fatalf("popped %q, want b", plan.Entry.ID)
	}
	kept := ApplyRemoval(entries, plan)
	if got := LeafOf(kept); got != "a" {
		t.Fatalf("the branch tip is %q, want a — it was left pointing at the removed entry", got)
	}
}

// "My last message" is on the branch I am on. An item on an abandoned attempt
// is already off the path and is not what anyone means.
func TestPlanPopLastItemStaysOnTheActiveBranch(t *testing.T) {
	entries := []Entry{
		item("q", "", 1),
		item("first", "q", 2),
		mustLeaf(t, "l", "q"), // go back to the question
		item("second", "q", 4),
	}
	plan, ok := PlanPop(entries, PopLastItem)
	if !ok {
		t.Fatal("nothing popped")
	}
	if plan.Entry.ID != "second" {
		t.Fatalf("popped %q, want the newest item on the active branch", plan.Entry.ID)
	}
}

func TestPlanPopOnAnEmptySession(t *testing.T) {
	if _, ok := PlanPop(nil, PopLast); ok {
		t.Fatal("an empty session popped something")
	}
	if _, ok := PlanPop([]Entry{ann("c", "", 1)}, PopLastItem); ok {
		t.Fatal("a session with no items popped an item")
	}
}
