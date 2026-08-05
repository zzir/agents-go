package memory

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestFileSession_RoundTrip(t *testing.T) {
	ctx := context.Background()
	sess, err := NewFileSession(t.TempDir(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}

	items := agents.InputItemsFromText("hello")
	items = append(items, agents.InputItemsFromText("world")...)
	if err := agents.NewSession(sess).AppendItems(ctx, items, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	got, err := agents.NewSession(sess).ContextItems(ctx, agents.Cursor{})
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
	last, err := agents.NewSession(sess).ContextItems(ctx, agents.Cursor{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].OfMessage.Content.OfString.Value != "world" {
		t.Errorf("limit-1 = %+v", last)
	}

	// Pop removes the most recent.
	popped, err := sess.PopEntry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	poppedItem, perr := popped.InputItem()
	if perr != nil {
		t.Fatal(perr)
	}
	if popped == nil || poppedItem.OfMessage.Content.OfString.Value != "world" {
		t.Errorf("popped = %+v", popped)
	}
	remaining, _ := agents.NewSession(sess).ContextItems(ctx, agents.Cursor{})
	if len(remaining) != 1 {
		t.Errorf("after pop: %d items, want 1", len(remaining))
	}

	// Clear empties the session.
	if err := sess.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	empty, _ := agents.NewSession(sess).ContextItems(ctx, agents.Cursor{})
	if len(empty) != 0 {
		t.Errorf("after clear: %d items, want 0", len(empty))
	}
}

func TestFileSession_PersistsAcrossInstances(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	a, err := NewFileSession(dir, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := agents.NewSession(a).AppendItems(ctx, agents.InputItemsFromText("remember me"), agents.Source{}); err != nil {
		t.Fatal(err)
	}

	// A fresh instance pointing at the same dir/session must see the history.
	b, err := NewFileSession(dir, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	items, _ := agents.NewSession(b).ContextItems(ctx, agents.Cursor{})
	if len(items) != 1 {
		t.Errorf("reopened session lost history: %d items", len(items))
	}
}

func TestFileSession_IsolationBySessionID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, _ := NewFileSession(dir, "a")
	b, _ := NewFileSession(dir, "b")

	if err := agents.NewSession(a).AppendItems(ctx, agents.InputItemsFromText("for-a"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	bItems, _ := agents.NewSession(b).ContextItems(ctx, agents.Cursor{})
	if len(bItems) != 0 {
		t.Errorf("session b leaked items from a: %d", len(bItems))
	}
}

func TestFileSession_PopOnEmpty(t *testing.T) {
	ctx := context.Background()
	sess, _ := NewFileSession(t.TempDir(), "empty")
	item, err := sess.PopEntry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if item != nil {
		t.Errorf("pop on empty should return nil, got %+v", item)
	}
}

// A pop rewrites the file, and a rewrite computed from a lenient read destroys
// every line it could not decode — the only copy of whatever those lines held.
// So a pop on a file with a corrupt line REFUSES (spec §2.5e2: reading what is
// being removed and removing it are one step), and the file — corrupt line
// included — stays exactly as it was.
func TestFileSession_PopRefusesOnCorruptLine(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sess, err := NewFileSession(dir, "corrupt")
	if err != nil {
		t.Fatal(err)
	}
	items := append(agents.InputItemsFromText("one"), agents.InputItemsFromText("two")...)
	entries, err := agents.NewItemEntries(items, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(ctx, entries...); err != nil {
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
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sess.PopEntry(ctx); err == nil {
		t.Fatal("pop on a corrupt file succeeded; it would have silently destroyed the corrupt line")
	}
	if _, err := sess.PopItem(ctx); err == nil {
		t.Fatal("item pop on a corrupt file succeeded; it would have silently destroyed the corrupt line")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a refused pop still rewrote the file")
	}

	// Reading stays lenient: one bad record must not make the session
	// unreadable, and nothing is destroyed by a read.
	got, err := sess.Entries(ctx, agents.Cursor{})
	if err != nil || len(got) != 2 {
		t.Fatalf("lenient read after corruption: %d entries, err=%v", len(got), err)
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

// Two instances opened on the same path must share a lock: concurrent
// AddItems (O_APPEND) and PopItem (read+rename) from separate instances used
// to drop appended lines silently.
func TestFileSession_ConcurrentInstancesShareLock(t *testing.T) {
	dir := t.TempDir()
	a, err := NewFileSession(dir, "shared")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewFileSession(dir, "shared")
	if err != nil {
		t.Fatal(err)
	}

	const writes = 100
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range writes {
			if err := agents.NewSession(a).AppendItems(context.Background(), agents.InputItemsFromText("from-a"), agents.Source{}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var popped int
	go func() {
		defer wg.Done()
		for range writes / 4 {
			item, err := b.PopEntry(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if item != nil {
				popped++
			}
		}
	}()
	wg.Wait()

	items, err := agents.NewSession(a).ContextItems(context.Background(), agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(items) + popped; got != writes {
		t.Errorf("items+popped = %d, want %d (lost writes)", got, writes)
	}
}

// mustEntries wraps plain items as item entries for tests that exercise the
// replace path.
func mustEntries(t *testing.T, items []agents.InputItem) []agents.SessionEntry {
	t.Helper()
	entries, err := agents.NewItemEntries(items, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
