package agents

import (
	"context"
	"strings"
	"testing"
)

func appendText(t *testing.T, sess *Session, texts ...string) []SessionEntry {
	t.Helper()
	ctx := context.Background()
	for _, text := range texts {
		if err := sess.AppendItems(ctx, InputItemsFromText(text), Source{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func contextTexts(t *testing.T, sess *Session) []string {
	t.Helper()
	items, err := sess.ContextItems(context.Background(), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range items {
		raw, err := MarshalInputItem(item)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	return out
}

// Appends chain: each entry names the one before it, so the session is a walk
// rather than a pile.
func TestTree_AppendsChain(t *testing.T) {
	sess := NewInMemorySession()
	entries := appendText(t, sess, "a", "b", "c")

	if entries[0].ParentID != "" {
		t.Errorf("first entry has parent %q, want none", entries[0].ParentID)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].ParentID != entries[i-1].ID {
			t.Errorf("entry %d parent = %q, want %q", i, entries[i].ParentID, entries[i-1].ID)
		}
	}
	if leaf := LeafOf(entries); leaf != entries[2].ID {
		t.Errorf("leaf = %q, want the last entry", leaf)
	}
}

// The point of a tree: retrying from an earlier point keeps the abandoned
// attempt recorded but off the path the model sees.
func TestTree_BranchAbandonsWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()
	entries := appendText(t, sess, "question", "first answer")

	// Go back to the question and answer differently.
	if err := sess.Branch(ctx, entries[0].ID); err != nil {
		t.Fatal(err)
	}
	appendText(t, sess, "second answer")

	got := contextTexts(t, sess)
	if len(got) != 2 {
		t.Fatalf("context = %d items, want the question and the second answer: %v", len(got), got)
	}
	if !strings.Contains(got[1], "second answer") {
		t.Errorf("context ends with %q, want the second answer", got[1])
	}
	for _, item := range got {
		if strings.Contains(item, "first answer") {
			t.Error("the abandoned answer reached the model")
		}
	}

	// It is abandoned, not deleted — the log still has it.
	all, err := sess.Entries(ctx, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range all {
		if e.Kind == EntryKindItem && strings.Contains(string(e.Item), "first answer") {
			found = true
		}
	}
	if !found {
		t.Error("branching deleted the abandoned attempt; it should still be recorded")
	}
}

// Switching branches is itself an append, so the switch is part of the history
// and the leaf can be derived rather than stored.
func TestTree_BranchIsRecordedAsAnEntry(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()
	entries := appendText(t, sess, "a", "b")
	if err := sess.Branch(ctx, entries[0].ID); err != nil {
		t.Fatal(err)
	}

	all, _ := sess.Entries(ctx, Cursor{})
	var leaves int
	for _, e := range all {
		if e.Kind == EntryKindLeaf {
			leaves++
		}
	}
	if leaves != 1 {
		t.Errorf("leaf entries = %d, want 1 — the switch should be in the log", leaves)
	}
	if got, err := sess.Leaf(ctx); err != nil || got != entries[0].ID {
		t.Errorf("leaf = %q (%v), want the branch target", got, err)
	}
}

func TestTree_BranchRejectsUnknownEntry(t *testing.T) {
	sess := NewInMemorySession()
	appendText(t, sess, "a")
	if err := sess.Branch(context.Background(), "nope"); err == nil {
		t.Error("branching to an entry that does not exist should fail")
	}
}

// A compaction checkpoint does NOT end the walk: an entry it folded is still
// on the branch it was written to. What the MODEL sees is the projection's
// question — it drops what the checkpoint's exclusions name — and conflating
// the two is what once made everything behind a checkpoint unreachable to a
// pop while the model could still see it.
func TestTree_PathContinuesPastCompaction(t *testing.T) {
	ctx := context.Background()
	sess := NewInMemorySession()
	entries := appendText(t, sess, "ancient")
	cp, err := NewCompactionEntry(CompactionPayload{Summary: "SUMMARY", ExcludedIDs: []string{entries[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(ctx, cp); err != nil {
		t.Fatal(err)
	}
	appendText(t, sess, "recent")

	path, err := sess.PathEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 || path[1].Kind != EntryKindCompaction {
		t.Fatalf("path = %d entries, want the folded entry, the checkpoint and what followed", len(path))
	}
	for _, item := range contextTexts(t, sess) {
		if strings.Contains(item, "ancient") {
			t.Error("history folded away by compaction came back")
		}
	}
}

// A fork extracts the active branch, not everything: the point is to continue
// from where the conversation actually is.
func TestFork_ExtractsTheActiveBranch(t *testing.T) {
	ctx := context.Background()
	src := NewInMemorySession()
	entries := appendText(t, src, "question", "abandoned")
	if err := src.Branch(ctx, entries[0].ID); err != nil {
		t.Fatal(err)
	}
	appendText(t, src, "kept")

	dst := NewInMemorySession()
	if err := ForkSession(ctx, src, dst); err != nil {
		t.Fatal(err)
	}

	got := contextTexts(t, dst)
	if len(got) != 2 {
		t.Fatalf("fork = %d items, want the branch: %v", len(got), got)
	}
	for _, item := range got {
		if strings.Contains(item, "abandoned") {
			t.Error("the fork carried an abandoned branch across")
		}
	}

	// Entry ids survive, so an update entry naming one still finds it.
	forked, _ := dst.Entries(ctx, Cursor{})
	if forked[0].ID != entries[0].ID {
		t.Errorf("fork changed entry ids (%q -> %q); updates would lose their targets",
			entries[0].ID, forked[0].ID)
	}

	// And the fork is independent: writing to it does not touch the source.
	appendText(t, dst, "only in the fork")
	if len(contextTexts(t, src)) != 2 {
		t.Error("writing to the fork changed the source")
	}
}
