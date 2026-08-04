package compaction

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// dropOldest excludes the first group, so a pass always has something to
// checkpoint.
type dropOldest struct{}

func (dropOldest) Compact(_ context.Context, idx *Index) (bool, error) {
	for _, g := range idx.Groups {
		if !g.Excluded {
			g.Excluded = true
			g.ExcludeReason = "test"
			return true, nil
		}
	}
	return false, nil
}

// A shared Compactor raced by another session must LOSE its checkpoint, never
// write the other session's. Compact and Checkpoint are two lock
// acquisitions; between one run's pass and its checkpoint, another run's pass
// can re-aim the shared index at a different conversation — and recording that
// state would durably write session B's exclusions and fold content into
// session A's log.
func TestCheckpointRefusesAnotherSessionsPass(t *testing.T) {
	ctx := context.Background()
	c := New(dropOldest{}, nil)

	sessionA := []agents.SessionEntry{
		userWithID(t, "e1", "a-one"), userWithID(t, "e2", "a-two"), userWithID(t, "e3", "a-three"),
	}
	if _, err := c.Compact(ctx, sessionA); err != nil {
		t.Fatal(err)
	}

	// The interleaved pass: another run re-aims the shared index.
	sessionB := []agents.SessionEntry{
		userWithID(t, "e1", "b-SECRET-one"), userWithID(t, "e2", "b-two"),
	}
	if _, err := c.Compact(ctx, sessionB); err != nil {
		t.Fatal(err)
	}

	// Session A asks for the checkpoint of ITS pass: refused, not leaked.
	if e, ok, err := c.Checkpoint(sessionA); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("checkpoint for a superseded pass was produced: %+v — session B's state would land in session A's log", e)
	}

	// The run whose pass the index still describes gets its checkpoint.
	if _, ok, err := c.Checkpoint(sessionB); err != nil || !ok {
		t.Fatalf("checkpoint for the current pass refused: ok=%v err=%v", ok, err)
	}
}

// Excluding a group must LOWER ContextTokens even when the newest entry
// carries measured usage: the measurement predates the exclusion, so the
// excluded group is subtracted at its estimate. A number exclusion cannot
// move keeps every trigger firing to the preserve floor and makes
// a tokens-below Target unreachable.
func TestContextTokensFallWithExclusions(t *testing.T) {
	entries := []agents.SessionEntry{
		userWithID(t, "e1", "an old question with plenty of text in it"),
		userWithID(t, "e2", "another old question with plenty of text"),
		userWithID(t, "e3", "the newest question"),
	}
	entries[2].Usage = &agents.RequestUsage{InputTokens: 10_000, OutputTokens: 50, TotalTokens: 10_050}

	idx := NewIndex(entries, nil)
	before := idx.ContextTokens()
	if before != 10_050 {
		t.Fatalf("baseline = %d, want the measured 10050", before)
	}

	idx.Groups[0].Excluded = true
	after := idx.ContextTokens()
	if after >= before {
		t.Fatalf("ContextTokens did not fall on exclusion: %d -> %d", before, after)
	}

	// A later model call prices the exclusion in: its usage measured the view
	// WITHOUT the excluded group, so the subtraction must stop — or the group
	// would be discounted twice.
	next := userWithID(t, "e4", "a follow-up")
	next.Usage = &agents.RequestUsage{InputTokens: 6_000, OutputTokens: 40, TotalTokens: 6_040}
	idx.Update(append(append([]agents.SessionEntry{}, entries...), next))
	settled := idx.ContextTokens()
	if settled != 6_040 {
		t.Fatalf("settled exclusion still subtracted: got %d, want the new measurement 6040", settled)
	}
}
