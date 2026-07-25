package memory

import (
	"context"
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
	if err := agents.AddSessionItems(ctx, sess, items, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	got, err := agents.SessionItems(ctx, sess, 0)
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
	last, err := agents.SessionItems(ctx, sess, 1)
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
	remaining, _ := agents.SessionItems(ctx, sess, 0)
	if len(remaining) != 1 {
		t.Errorf("after pop: %d items, want 1", len(remaining))
	}

	// Clear empties the session.
	if err := sess.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	empty, _ := agents.SessionItems(ctx, sess, 0)
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
	if err := agents.AddSessionItems(ctx, a, agents.InputItemsFromText("remember me"), agents.Source{}); err != nil {
		t.Fatal(err)
	}

	// A fresh instance pointing at the same dir/session must see the history.
	b, err := NewFileSession(dir, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	items, _ := agents.SessionItems(ctx, b, 0)
	if len(items) != 1 {
		t.Errorf("reopened session lost history: %d items", len(items))
	}
}

func TestFileSession_IsolationBySessionID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, _ := NewFileSession(dir, "a")
	b, _ := NewFileSession(dir, "b")

	if err := agents.AddSessionItems(ctx, a, agents.InputItemsFromText("for-a"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	bItems, _ := agents.SessionItems(ctx, b, 0)
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
			if err := agents.AddSessionItems(context.Background(), a, agents.InputItemsFromText("from-a"), agents.Source{}); err != nil {
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

	items, err := agents.SessionItems(context.Background(), a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(items) + popped; got != writes {
		t.Errorf("items+popped = %d, want %d (lost writes)", got, writes)
	}
}

// mustEntries wraps plain items as item entries for tests that exercise the
// replace path.
func mustEntries(t *testing.T, items []agents.TResponseInputItem) []agents.SessionEntry {
	t.Helper()
	entries, err := agents.NewItemEntries(items, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
