package filesession

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

func TestStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}

	items := agents.InputItemsFromText("hello")
	items = append(items, agents.InputItemsFromText("world")...)
	if err := session.NewSession(store).AppendItems(ctx, items, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	got, err := session.NewSession(store).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0].OfMessage == nil || got[0].OfMessage.Content.OfString.Value != "hello" {
		t.Errorf("first item = %+v", got[0])
	}

	// Limit returns the most recent N, oldest-first.
	last, err := session.NewSession(store).ContextItems(ctx, session.Cursor{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].OfMessage.Content.OfString.Value != "world" {
		t.Errorf("limit-1 = %+v", last)
	}

	// Clear empties the session.
	if err := store.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	empty, _ := session.NewSession(store).ContextItems(ctx, session.Cursor{})
	if len(empty) != 0 {
		t.Errorf("after clear: %d items, want 0", len(empty))
	}
}

func TestStore_PersistsAcrossInstances(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	a, err := New(dir, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewSession(a).AppendItems(ctx, agents.InputItemsFromText("remember me"), agents.Source{}); err != nil {
		t.Fatal(err)
	}

	// A fresh instance pointing at the same dir/session must see the history.
	b, err := New(dir, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	items, _ := session.NewSession(b).ContextItems(ctx, session.Cursor{})
	if len(items) != 1 {
		t.Errorf("reopened session lost history: %d items", len(items))
	}
}

func TestStore_IsolationBySessionID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, _ := New(dir, "a")
	b, _ := New(dir, "b")

	if err := session.NewSession(a).AppendItems(ctx, agents.InputItemsFromText("for-a"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	bItems, _ := session.NewSession(b).ContextItems(ctx, session.Cursor{})
	if len(bItems) != 0 {
		t.Errorf("session b leaked items from a: %d", len(bItems))
	}
}

// One bad record must not make the session unreadable, and nothing is
// destroyed by a read.
func TestStore_LenientReadOnCorruptLine(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := New(dir, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	items := append(agents.InputItemsFromText("one"), agents.InputItemsFromText("two")...)
	entries, err := session.NewItemEntries(items, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, entries...); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "corrupt.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{this is not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := store.Entries(ctx, session.Cursor{})
	if err != nil || len(got) != 2 {
		t.Fatalf("lenient read after corruption: %d entries, err=%v", len(got), err)
	}
}

// An append reads only the file's last line to find the branch tip. A leaf move
// is a marker rather than a node, so the tip it names is its TARGET — reading
// the marker itself as the tip would parent the next entry on the switch, and
// the branch the caller switched to would end there.
func TestStore_AppendLinksToALeafMoveTarget(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir(), "branch")
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.NewItemEntries(agents.InputItemsFromText("one"), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.NewItemEntries(agents.InputItemsFromText("two"), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, first...); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, second...); err != nil {
		t.Fatal(err)
	}

	stored, err := store.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("got %d entries, want 2", len(stored))
	}
	rootID := stored[0].ID

	// Switch the active branch back to the first entry, then extend it.
	move, err := session.NewLeafEntry(rootID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, move); err != nil {
		t.Fatal(err)
	}
	third, err := session.NewItemEntries(agents.InputItemsFromText("three"), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, third...); err != nil {
		t.Fatal(err)
	}

	stored, err = store.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	last := stored[len(stored)-1]
	if last.ParentID != rootID {
		t.Errorf("the entry after the leaf move is parented at %q, want the move's target %q", last.ParentID, rootID)
	}
	branch := session.PathToLeaf(stored, session.LeafOf(stored))
	if len(branch) != 2 || branch[0].ID != rootID || branch[1].ID != last.ID {
		t.Errorf("branch is %d entries, want the root plus the entry appended after the switch", len(branch))
	}
	// Sequence numbers still climb: the leaf move carries one too, and the tip
	// read must not hand its number out again.
	for i := 1; i < len(stored); i++ {
		if stored[i].Seq <= stored[i-1].Seq {
			t.Fatalf("entry %d has seq %d, not past its predecessor's %d", i, stored[i].Seq, stored[i-1].Seq)
		}
	}
}

// Reads skip a line they cannot decode; the tip read must skip it the same way,
// or an append lands parented on nothing and starts a second root.
func TestStore_AppendSkipsACorruptLastLine(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := New(dir, "corrupt-tail")
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.NewItemEntries(agents.InputItemsFromText("one"), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, first...); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	rootID := stored[0].ID

	f, err := os.OpenFile(filepath.Join(dir, "corrupt-tail.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{this is not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := session.NewItemEntries(agents.InputItemsFromText("two"), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, second...); err != nil {
		t.Fatal(err)
	}
	stored, err = store.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("got %d entries, want 2", len(stored))
	}
	if stored[1].ParentID != rootID {
		t.Errorf("entry appended past a corrupt line is parented at %q, want %q", stored[1].ParentID, rootID)
	}
}

// A leaf move whose payload will not decode names no target, so folding the log
// leaves the tip where the entry before it put it. Nothing rejects such an entry
// — PrepareAppend tolerates the decode failure — so the tip read has to walk
// past it too, or the next append starts a second, detached root and the branch
// walk returns that entry alone.
func TestStore_AppendSkipsAnUndecodableLeafMove(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir(), "bad-leaf-tail")
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.NewItemEntries(agents.InputItemsFromText("one"), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, first...); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	rootID := stored[0].ID

	// Built by hand rather than through NewLeafEntry, so it carries no payload.
	if err := store.Append(ctx, session.Entry{Kind: session.EntryKindLeaf}); err != nil {
		t.Fatal(err)
	}
	second, err := session.NewItemEntries(agents.InputItemsFromText("two"), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, second...); err != nil {
		t.Fatal(err)
	}

	stored, err = store.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	last := stored[len(stored)-1]
	if last.ParentID != rootID {
		t.Errorf("entry appended past a payload-less leaf move is parented at %q, want %q", last.ParentID, rootID)
	}
	if branch := session.PathToLeaf(stored, session.LeafOf(stored)); len(branch) != 2 {
		t.Errorf("branch is %d entries, want both items still on it", len(branch))
	}
}

func TestSanitizeSessionID(t *testing.T) {
	cases := map[string]string{
		"plain":         "plain",
		"user/123":      "user_123",
		"../etc/passwd": "_etc_passwd", // leading dots are trimmed (safer)
		"a b":           "a_b",
	}
	for in, want := range cases {
		if got := sanitizeSessionID(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if sanitizeSessionID("") != "" || sanitizeSessionID("..") != "" {
		t.Error("empty/.. should sanitize to empty")
	}
	// A sanitized id must not contain a path separator.
	if filepath.Base(sanitizeSessionID("a/b/c")) != sanitizeSessionID("a/b/c") {
		t.Error("sanitized id should be a single path component")
	}
}

// Two instances opened on the same path must share a lock: an append reads the
// tip (parent id, last seq) and then writes, so two unserialized instances
// hand out the same parent and the same sequence numbers.
func TestStore_ConcurrentInstancesShareLock(t *testing.T) {
	dir := t.TempDir()
	a, err := New(dir, "shared")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(dir, "shared")
	if err != nil {
		t.Fatal(err)
	}

	const writes = 100
	var wg sync.WaitGroup
	wg.Add(2)
	for _, st := range []*Store{a, b} {
		go func() {
			defer wg.Done()
			for range writes {
				if err := session.NewSession(st).AppendItems(context.Background(), agents.InputItemsFromText("hi"), agents.Source{}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	entries, err := a.Entries(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2*writes {
		t.Errorf("stored %d entries, want %d (lost writes)", len(entries), 2*writes)
	}
	seen := map[int64]bool{}
	for _, e := range entries {
		if seen[e.Seq] {
			t.Fatalf("sequence number %d was issued twice", e.Seq)
		}
		seen[e.Seq] = true
	}
}

// mustEntries wraps plain items as item entries for tests that exercise the
// replace path.
func mustEntries(t *testing.T, items []agents.InputItem) []session.Entry {
	t.Helper()
	entries, err := session.NewItemEntries(items, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
