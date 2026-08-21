package agentstest

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
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
func StorageConformance(t *testing.T, newStorage func(t *testing.T) session.Storage) {
	t.Helper()
	for _, c := range storageChecks {
		t.Run(c.name, func(t *testing.T) { c.run(t, newStorage(t)) })
	}
}

var storageChecks = []struct {
	name string
	run  func(t *testing.T, st session.Storage)
}{
	{"SeqIsMonotonic", checkSeqMonotonic},
	{"SeqSurvivesAReplace", checkSeqSurvivesReplace},
	{"AReplaceKeepsTheIDsItIsGiven", checkReplaceKeepsIDs},
	{"AGuardedReplaceTakesTheSeqItWasShown", checkGuardedReplaceMatches},
	{"AGuardedReplaceRefusesAMovedLog", checkGuardedReplaceRefusesMoved},
	{"AGuardedReplaceOnAnEmptyLogExpectsZero", checkGuardedReplaceEmptyLog},
	{"SeqDoesNotMoveOnRead", checkSeqStableOnRead},
	{"EntryIDsAreUniqueAndNotReused", checkEntryIDsUnique},
	{"CursorReturnsWhatItHasNotShown", checkCursorCompleteness},
}

func storageWrite(t *testing.T, st session.Storage, texts ...string) {
	t.Helper()
	entries := make([]session.Entry, 0, len(texts))
	for _, text := range texts {
		entries = append(entries, storageItem(t, text))
	}
	if err := st.Append(context.Background(), entries...); err != nil {
		t.Fatalf("append %v: %v", texts, err)
	}
}

// storageItem builds one unstored user-message entry.
func storageItem(t *testing.T, text string) session.Entry {
	t.Helper()
	item, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"` + text + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := session.NewItemEntry(item, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func storageEntries(t *testing.T, st session.Storage) []session.Entry {
	t.Helper()
	got, err := st.Entries(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	return got
}

func checkSeqMonotonic(t *testing.T, st session.Storage) {
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

// Clearing or replacing a history does not restart the numbering: a cursor
// outlives the entries it pointed at.
func checkSeqSurvivesReplace(t *testing.T, st session.Storage) {
	t.Helper()
	ctx := context.Background()
	replacer, ok := st.(session.AtomicReplacer)
	if !ok {
		t.Skip("this store does not replace its history")
	}
	storageWrite(t, st, "one", "two")
	highest := storageEntries(t, st)[1].Seq

	if err := replacer.ReplaceEntries(ctx, storageItem(t, "replacement")); err != nil {
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

// A replace keeps the ids it is given. A rewrite that carries entries over —
// server-side compaction keeps everything it did not summarize — hands them
// back as it read them, and an update entry names its target by id: a store
// that re-mints on the way through leaves the update pointing at an entry no
// longer there, and a fold that finds no target is dropped in silence.
func checkReplaceKeepsIDs(t *testing.T, st session.Storage) {
	t.Helper()
	ctx := context.Background()
	replacer, ok := st.(session.AtomicReplacer)
	if !ok {
		t.Skip("this store does not replace its history")
	}
	storageWrite(t, st, "one", "two")
	kept := storageEntries(t, st)[1]

	// An entry as a rewrite hands it back: identity intact, and the fields the
	// store owns left for the store to fill in again.
	carried := kept
	carried.ParentID, carried.Seq = "", 0
	if err := replacer.ReplaceEntries(ctx, carried); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got := storageEntries(t, st)
	if len(got) != 1 {
		t.Fatalf("after a replace the session holds %d entries, want 1", len(got))
	}
	if got[0].ID != kept.ID {
		t.Fatalf("the replace re-minted the entry id: %q came back as %q", kept.ID, got[0].ID)
	}
	if got[0].Seq <= kept.Seq {
		t.Fatalf("the carried entry has Seq %d, at or before the %d already issued", got[0].Seq, kept.Seq)
	}

	g, ok := st.(session.GuardedReplacer)
	if !ok {
		return
	}
	carried.ParentID, carried.Seq = "", 0
	replaced, err := g.ReplaceEntriesIf(ctx, got[0].Seq, carried)
	if err != nil {
		t.Fatalf("guarded replace: %v", err)
	}
	if !replaced {
		t.Fatalf("the guarded replace refused Seq %d, which is where the log stands", got[0].Seq)
	}
	if again := storageEntries(t, st); again[0].ID != kept.ID {
		t.Fatalf("the guarded replace re-minted the entry id: %q came back as %q", kept.ID, again[0].ID)
	}
}

// A guarded replace is the ordinary swap when the log has not moved: the
// sequence number the caller was shown is still the highest one there.
func checkGuardedReplaceMatches(t *testing.T, st session.Storage) {
	t.Helper()
	ctx := context.Background()
	g, ok := st.(session.GuardedReplacer)
	if !ok {
		t.Skip("this store does not replace its history under a guard")
	}
	storageWrite(t, st, "one", "two")
	expect := storageEntries(t, st)[1].Seq

	replaced, err := g.ReplaceEntriesIf(ctx, expect, storageItem(t, "replacement"))
	if err != nil {
		t.Fatalf("guarded replace: %v", err)
	}
	if !replaced {
		t.Fatalf("the guarded replace refused Seq %d, which is where the log stands", expect)
	}
	got := storageEntries(t, st)
	if len(got) != 1 {
		t.Fatalf("after a guarded replace the session holds %d entries, want 1", len(got))
	}
	if got[0].Seq <= expect {
		t.Fatalf("the replacement has Seq %d, at or before the %d already issued", got[0].Seq, expect)
	}
}

// The whole point: a log that moved since the caller read it is left EXACTLY as
// it stands. The replacement was computed from a history that no longer exists,
// and writing it would delete what arrived in the meantime — the entries the
// caller never saw, held nowhere else.
func checkGuardedReplaceRefusesMoved(t *testing.T, st session.Storage) {
	t.Helper()
	ctx := context.Background()
	g, ok := st.(session.GuardedReplacer)
	if !ok {
		t.Skip("this store does not replace its history under a guard")
	}
	storageWrite(t, st, "one", "two")
	stale := storageEntries(t, st)[1].Seq
	storageWrite(t, st, "three")
	before := storageEntries(t, st)

	replaced, err := g.ReplaceEntriesIf(ctx, stale, storageItem(t, "replacement"))
	if err != nil {
		t.Fatalf("guarded replace: %v", err)
	}
	if replaced {
		t.Fatalf("the guarded replace wrote against Seq %d after the log moved past it", stale)
	}
	after := storageEntries(t, st)
	if len(after) != len(before) {
		t.Fatalf("a refused replace left %d entries, want the %d that were there", len(after), len(before))
	}
	for i := range before {
		if !before[i].Equal(after[i]) {
			t.Fatalf("a refused replace changed entry %d: %+v became %+v", i, before[i], after[i])
		}
	}
}

// Zero is the expectation for a log read empty. It is what the log HOLDS, not
// what the store has ever handed out: a store answering with its high-water
// mark would refuse every replace of a session that had been emptied, forever.
func checkGuardedReplaceEmptyLog(t *testing.T, st session.Storage) {
	t.Helper()
	ctx := context.Background()
	g, ok := st.(session.GuardedReplacer)
	if !ok {
		t.Skip("this store does not replace its history under a guard")
	}
	replaced, err := g.ReplaceEntriesIf(ctx, 0, storageItem(t, "first"))
	if err != nil {
		t.Fatalf("guarded replace of an empty log: %v", err)
	}
	if !replaced {
		t.Fatal("the guarded replace refused an empty log at expect 0")
	}
	if n := len(storageEntries(t, st)); n != 1 {
		t.Fatalf("after the replace the session holds %d entries, want 1", n)
	}

	// And once it holds something, zero is a stale expectation like any other.
	replaced, err = g.ReplaceEntriesIf(ctx, 0, storageItem(t, "second"))
	if err != nil {
		t.Fatalf("guarded replace at a stale zero: %v", err)
	}
	if replaced {
		t.Fatal("the guarded replace accepted expect 0 against a log that holds an entry")
	}
}

// Reading a session does not renumber it. A store that numbers by position in
// the result set moves every surviving entry whenever a read filters one out.
func checkSeqStableOnRead(t *testing.T, st session.Storage) {
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
	tail, err := st.Entries(context.Background(), session.Cursor{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) == 1 && tail[0].Seq != first[len(first)-1].Seq {
		t.Fatalf("the last entry is Seq %d in a full read and %d in a partial one",
			first[len(first)-1].Seq, tail[0].Seq)
	}
}

func checkEntryIDsUnique(t *testing.T, st session.Storage) {
	t.Helper()
	storageWrite(t, st, "one", "two")
	storageWrite(t, st, "three")

	seen := map[string]bool{}
	for _, e := range storageEntries(t, st) {
		if seen[e.ID] {
			t.Fatalf("entry id %q appears twice", e.ID)
		}
		seen[e.ID] = true
	}
}

// The point of a cursor: resuming from the last number seen returns everything
// since, and nothing already shown.
func checkCursorCompleteness(t *testing.T, st session.Storage) {
	t.Helper()
	ctx := context.Background()
	storageWrite(t, st, "one", "two")
	seen := storageEntries(t, st)
	cursor := seen[len(seen)-1].Seq

	storageWrite(t, st, "three")

	fresh, err := st.Entries(ctx, session.Cursor{AfterSeq: cursor})
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
