package agents

import (
	"testing"
)

// withClock makes the allocator's clock predictable so a test can state what it
// expects instead of describing it.
func withClock(t *testing.T, at int64) {
	t.Helper()
	prev := nowNanos
	nowNanos = func() int64 { return at }
	t.Cleanup(func() { nowNanos = prev })
}

// The three properties a Cursor depends on, stated one at a time.
func TestSeqForIsNeverReused(t *testing.T) {
	withClock(t, 1000)
	// A session that issued up to 5 and then had those entries removed: the
	// next number is still past everything it ever handed out, because the
	// clock does not go back when an entry does.
	if got := SeqFor(AppendPoint{LastSeq: 5}); got != 1000 {
		t.Fatalf("SeqFor after a removal = %d, want the clock's 1000", got)
	}
}

func TestSeqForSurvivesAClockThatStepsBack(t *testing.T) {
	withClock(t, 900)
	// The session has already issued 1000, so 900 is not available whatever the
	// clock says.
	if got := SeqFor(AppendPoint{LastSeq: 1000}); got != 1001 {
		t.Fatalf("SeqFor with a stepped-back clock = %d, want 1001", got)
	}
}

func TestSeqForOnAFreshSession(t *testing.T) {
	withClock(t, 1000)
	if got := SeqFor(AppendPoint{}); got != 1000 {
		t.Fatalf("SeqFor on an empty session = %d, want 1000", got)
	}
}

// Two appends inside one nanosecond must not collide, which is the same guard
// seen from the other side: the second one's append point already holds the
// first one's number.
func TestPrepareAppendNumbersWithinABatchAndAcrossOne(t *testing.T) {
	withClock(t, 1000)
	first := PrepareAppend([]SessionEntry{{}, {}}, AppendPoint{})
	if first[0].Seq != 1000 || first[1].Seq != 1001 {
		t.Fatalf("batch seqs = %d,%d, want 1000,1001", first[0].Seq, first[1].Seq)
	}
	second := PrepareAppend([]SessionEntry{{}}, AppendPointOf(first))
	if second[0].Seq != 1002 {
		t.Fatalf("next batch in the same nanosecond = %d, want 1002", second[0].Seq)
	}
}

// An id is derived from the sequence number, so "never handed out twice" is one
// property rather than two that have to agree.
func TestPrepareAppendDerivesIDsFromSeq(t *testing.T) {
	withClock(t, 1000)
	got := PrepareAppend([]SessionEntry{{}, {}}, AppendPoint{})
	for i := range got {
		if want := EntryIDFor(got[i].Seq); got[i].ID != want {
			t.Fatalf("entry %d id = %q, want %q", i, got[i].ID, want)
		}
	}
	if got[0].ID == got[1].ID {
		t.Fatal("two entries in one batch share an id")
	}
}

// An entry that arrives with an id keeps it: a fork or a replace re-adding a
// known entry keeps the identity an update entry points at.
func TestPrepareAppendKeepsAnExistingID(t *testing.T) {
	withClock(t, 1000)
	got := PrepareAppend([]SessionEntry{{ID: "carried-over"}}, AppendPoint{})
	if got[0].ID != "carried-over" {
		t.Fatalf("id = %q, want the one it arrived with", got[0].ID)
	}
	if got[0].Seq != 1000 {
		t.Fatalf("seq = %d, want a freshly allocated one", got[0].Seq)
	}
}

// Parent links chain within a batch and start from the append point.
func TestPrepareAppendLinksToTheAppendPoint(t *testing.T) {
	withClock(t, 1000)
	got := PrepareAppend([]SessionEntry{{}, {}}, AppendPoint{Leaf: "tip"})
	if got[0].ParentID != "tip" {
		t.Fatalf("first parent = %q, want the leaf it extends", got[0].ParentID)
	}
	if got[1].ParentID != got[0].ID {
		t.Fatalf("second parent = %q, want %q", got[1].ParentID, got[0].ID)
	}
}

// AppendPointOf reads the high-water mark, not the last entry's number: they
// differ when entries arrive out of order, and taking the last one would hand
// out a number the session has used.
func TestAppendPointOfTakesTheHighestSeq(t *testing.T) {
	at := AppendPointOf([]SessionEntry{{ID: "a", Seq: 9}, {ID: "b", Seq: 4}})
	if at.LastSeq != 9 {
		t.Fatalf("LastSeq = %d, want the highest", at.LastSeq)
	}
}
