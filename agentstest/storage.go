package agentstest

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// StorageConformance holds a SessionStorage to the entry-lifecycle contract in
// docs/spec.md §2.5e2.
//
// It exists because the contract is mostly implemented by shared code, and
// shared code only helps if every backend actually routes through it. A backend
// that reimplements one of these answers is a defect even when its answer looks
// right, because the next backend will answer differently — which is what four
// implementations did to every rule in that section before it was written down.
//
// newStorage must return an empty store, and must be callable repeatedly within
// one test.
func StorageConformance(t *testing.T, newStorage func(t *testing.T) agents.SessionStorage) {
	t.Helper()
	for _, c := range storageChecks {
		t.Run(c.name, func(t *testing.T) { c.run(t, newStorage(t)) })
	}
}

var storageChecks = []struct {
	name string
	run  func(t *testing.T, st agents.SessionStorage)
}{
	{"SeqIsMonotonic", checkSeqMonotonic},
	{"SeqSurvivesARemoval", checkSeqSurvivesRemoval},
	{"SeqSurvivesAReplace", checkSeqSurvivesReplace},
	{"SeqDoesNotMoveOnRead", checkSeqStableOnRead},
	{"EntryIDsAreUniqueAndNotReused", checkEntryIDsUnique},
	{"CursorReturnsWhatItHasNotShown", checkCursorCompleteness},
	{"PopTakesTheMostRecentEntry", checkPopTakesNewest},
	{"AnItemPopLeavesTheTreeWhole", checkItemPopKeepsTheTree},
	{"AnItemPopSkipsFoldedAndReachesKept", checkItemPopSkipsFoldedReachesKept},
	{"PoppingACheckpointUndoesItsFold", checkPopEntryTakesCheckpointAndUnfolds},
}

// A checkpoint reshapes what a pop can reach exactly as it reshapes the
// model's view: an entry it folded is skipped — it is no more "my last
// message" than a banner is — while the entries it KEPT stay reachable.
// Stopping the search at the checkpoint once made the kept entries unpoppable
// while the model could still see them.
func checkItemPopSkipsFoldedReachesKept(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	popper, ok := st.(agents.ItemPopper)
	if !ok {
		t.Skip("this store does not pop items")
	}
	storageWrite(t, st, "folded away", "kept in the window")
	stored := storageEntries(t, st)
	cp, err := agents.NewCompactionEntry(agents.CompactionPayload{
		Summary:     "summary of the folded part",
		ExcludedIDs: []string{stored[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(ctx, cp); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}

	popped, err := popper.PopItem(ctx)
	if err != nil {
		t.Fatalf("pop item: %v", err)
	}
	if popped == nil || popped.ID != stored[1].ID {
		t.Fatalf("popped %+v, want the kept item %q — the checkpoint must not wall it off", popped, stored[1].ID)
	}

	// The only item left is the folded one, which is not poppable: it is not
	// part of the conversation as anyone sees it.
	again, err := popper.PopItem(ctx)
	if err != nil {
		t.Fatalf("second pop item: %v", err)
	}
	if again != nil {
		t.Fatalf("popped folded entry %+v; folded history is not \"my last message\"", again)
	}
}

// PopEntry takes the most recent entry whatever it is — and when that is a
// compaction checkpoint, removing it undoes the fold: the exclusions leave
// with the checkpoint, so the folded history is part of the view again. The
// checkpoint and any store-side bookkeeping of the fold are two records of one
// fact, and a store that keeps such bookkeeping reverses it in the same step.
func checkPopEntryTakesCheckpointAndUnfolds(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	popper, ok := st.(agents.EntryPopper)
	if !ok {
		t.Skip("this store does not pop")
	}
	storageWrite(t, st, "the folded question")
	stored := storageEntries(t, st)
	cp, err := agents.NewCompactionEntry(agents.CompactionPayload{
		Summary:     "summary standing in for it",
		ExcludedIDs: []string{stored[0].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(ctx, cp); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}

	sess := agents.NewSession(st)
	before, err := sess.ContextEntries(ctx, agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range before {
		if e.ID == stored[0].ID {
			t.Fatalf("folded entry %q still in the context view before the pop", e.ID)
		}
	}

	popped, err := popper.PopEntry(ctx)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if popped == nil || popped.Kind != agents.EntryKindCompaction {
		t.Fatalf("popped %+v, want the checkpoint — it is the most recent entry", popped)
	}

	after, err := sess.ContextEntries(ctx, agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	restored := false
	for _, e := range after {
		if e.ID == stored[0].ID {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("the folded entry did not return when its checkpoint was popped; view = %+v", after)
	}
}

func storageWrite(t *testing.T, st agents.SessionStorage, texts ...string) {
	t.Helper()
	entries := make([]agents.SessionEntry, 0, len(texts))
	for _, text := range texts {
		item, err := agents.UnmarshalInputItem([]byte(`{"role":"user","content":"` + text + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		e, err := agents.NewItemEntry(item, agents.Source{})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	if err := st.Append(context.Background(), entries...); err != nil {
		t.Fatalf("append %v: %v", texts, err)
	}
}

func storageEntries(t *testing.T, st agents.SessionStorage) []agents.SessionEntry {
	t.Helper()
	got, err := st.Entries(context.Background(), agents.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	return got
}

func checkSeqMonotonic(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	storageWrite(t, st, "one", "two")
	storageWrite(t, st, "three")
	got := storageEntries(t, st)
	if len(got) != 3 {
		t.Fatalf("stored %d entries, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("entry %d has Seq %d, not past its predecessor's %d",
				i, got[i].Seq, got[i-1].Seq)
		}
	}
}

// A number this session has issued is never issued again, including after the
// entry holding it is removed. Otherwise a caller resuming from the last number
// it saw skips the next append forever — its cursor is already past it.
func checkSeqSurvivesRemoval(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	popper, ok := st.(agents.EntryPopper)
	if !ok {
		t.Skip("nothing here removes an entry, so no number is ever freed")
	}
	storageWrite(t, st, "one", "two")
	before := storageEntries(t, st)
	highest := before[len(before)-1].Seq

	if _, err := popper.PopEntry(ctx); err != nil {
		t.Fatalf("pop: %v", err)
	}
	storageWrite(t, st, "three")

	after := storageEntries(t, st)
	newest := after[len(after)-1]
	if newest.Seq <= highest {
		t.Fatalf("the entry appended after a pop has Seq %d, not past the %d already issued",
			newest.Seq, highest)
	}
}

// Clearing or replacing a history does not restart the numbering: a cursor
// outlives the entries it pointed at.
func checkSeqSurvivesReplace(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	replacer, ok := st.(agents.AtomicReplacer)
	if !ok {
		t.Skip("this store does not replace its history")
	}
	storageWrite(t, st, "one", "two")
	highest := storageEntries(t, st)[1].Seq

	item, err := agents.UnmarshalInputItem([]byte(`{"role":"user","content":"replacement"}`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := agents.NewItemEntry(item, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := replacer.ReplaceEntries(ctx, e); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got := storageEntries(t, st)
	if len(got) != 1 {
		t.Fatalf("after a replace the session holds %d entries, want 1", len(got))
	}
	if got[0].Seq <= highest {
		t.Fatalf("the replacement has Seq %d, at or before the %d already issued — a cursor would skip it",
			got[0].Seq, highest)
	}
}

// Reading a session does not renumber it. A store that numbers by position in
// the result set moves every surviving entry whenever a read filters one out.
func checkSeqStableOnRead(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	storageWrite(t, st, "one", "two", "three")
	first := storageEntries(t, st)
	second := storageEntries(t, st)
	for i := range first {
		if first[i].Seq != second[i].Seq {
			t.Fatalf("entry %q read back as Seq %d and then %d",
				first[i].ID, first[i].Seq, second[i].Seq)
		}
	}
	// And a read that returns a subset does not renumber what it returns.
	tail, err := st.Entries(context.Background(), agents.Cursor{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) == 1 && tail[0].Seq != first[len(first)-1].Seq {
		t.Fatalf("the last entry is Seq %d in a full read and %d in a partial one",
			first[len(first)-1].Seq, tail[0].Seq)
	}
}

func checkEntryIDsUnique(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	storageWrite(t, st, "one", "two")

	var popped string
	if popper, ok := st.(agents.EntryPopper); ok {
		e, err := popper.PopEntry(ctx)
		if err != nil {
			t.Fatalf("pop: %v", err)
		}
		if e != nil {
			popped = e.ID
		}
	}
	storageWrite(t, st, "three")

	seen := map[string]bool{}
	for _, e := range storageEntries(t, st) {
		if seen[e.ID] {
			t.Fatalf("entry id %q appears twice", e.ID)
		}
		seen[e.ID] = true
	}
	if popped != "" && seen[popped] {
		t.Fatalf("the popped entry's id %q was handed to a later entry", popped)
	}
}

// The point of a cursor: resuming from the last number seen returns everything
// since, and nothing already shown. Checked across a removal, which is where
// numbering by count or by position gets it wrong.
func checkCursorCompleteness(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	storageWrite(t, st, "one", "two")
	seen := storageEntries(t, st)
	cursor := seen[len(seen)-1].Seq

	if popper, ok := st.(agents.EntryPopper); ok {
		if _, err := popper.PopEntry(ctx); err != nil {
			t.Fatalf("pop: %v", err)
		}
	}
	storageWrite(t, st, "three")

	fresh, err := st.Entries(ctx, agents.Cursor{AfterSeq: cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatalf("resuming from Seq %d returned %d entries, want the one appended since", cursor, len(fresh))
	}
	for _, e := range fresh {
		if e.Seq <= cursor {
			t.Fatalf("resuming from Seq %d returned an entry at %d, which it had already been shown",
				cursor, e.Seq)
		}
	}
}

// EntryPopper takes the most recent ENTRY. A store that skips past what it
// finds uninteresting removes something else while reporting it popped the last
// thing — and leaves what it skipped pointing at what is now gone.
func checkPopTakesNewest(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	popper, ok := st.(agents.EntryPopper)
	if !ok {
		t.Skip("this store does not pop")
	}
	storageWrite(t, st, "one")
	// An entry that is not a conversation item, and is the most recent.
	if err := st.Append(ctx, agents.NewAnnotationEntry(
		agents.ItemDisplay{Kind: agents.DisplayError, Text: "boom"},
		agents.Source{Type: agents.SourceErrorHandler},
	)); err != nil {
		t.Fatalf("append annotation: %v", err)
	}
	stored := storageEntries(t, st)
	newest := stored[len(stored)-1]

	popped, err := popper.PopEntry(ctx)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if popped == nil {
		t.Fatal("nothing popped from a session with two entries")
	}
	if popped.ID != newest.ID {
		t.Fatalf("popped %q (kind %q), want the most recent entry %q (kind %q)",
			popped.ID, popped.Kind, newest.ID, newest.Kind)
	}
}

// Removing an entry from the middle of a branch — which is what an item pop
// does whenever something non-item sits above it — must not leave the
// survivors hanging off an id that is gone. A walk that meets a missing parent
// stops there, so the session would read short: losing everything BEFORE the
// entry that was removed, rather than just it.
func checkItemPopKeepsTheTree(t *testing.T, st agents.SessionStorage) {
	t.Helper()
	ctx := context.Background()
	popper, ok := st.(agents.ItemPopper)
	if !ok {
		t.Skip("this store does not pop items")
	}
	storageWrite(t, st, "one", "two")
	if err := st.Append(ctx, agents.NewAnnotationEntry(
		agents.ItemDisplay{Kind: agents.DisplayError, Text: "boom"},
		agents.Source{Type: agents.SourceErrorHandler},
	)); err != nil {
		t.Fatalf("append annotation: %v", err)
	}

	popped, err := popper.PopItem(ctx)
	if err != nil {
		t.Fatalf("pop item: %v", err)
	}
	if popped == nil || popped.Kind != agents.EntryKindItem {
		t.Fatalf("popped %+v, want the most recent item", popped)
	}

	kept := storageEntries(t, st)
	if len(kept) != 2 {
		t.Fatalf("session holds %d entries, want the first item and the annotation", len(kept))
	}
	byID := map[string]bool{}
	for _, e := range kept {
		byID[e.ID] = true
	}
	for _, e := range kept {
		if e.ParentID != "" && !byID[e.ParentID] {
			t.Fatalf("entry %q points at %q, which the pop removed", e.ID, e.ParentID)
		}
	}
	// The walk reaches every survivor: a walk that stops early is how a removal
	// in the middle of a branch loses everything BEFORE it.
	if n := len(agents.PathToLeaf(kept, agents.LeafOf(kept))); n != len(kept) {
		t.Fatalf("the active branch walks %d of %d surviving entries — the removal truncated it", n, len(kept))
	}
}
