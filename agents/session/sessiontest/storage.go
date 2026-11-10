package sessiontest

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// StorageConformance holds a Storage to the entry-lifecycle contract in spec
// §2.5e2; a backend answering a shared question itself fails even when the answer
// is right. newStorage returns an empty store and is callable repeatedly in one test.
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
	e, err := session.NewItemEntry(item, session.Source{})
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
