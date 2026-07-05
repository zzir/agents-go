package memory

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestFileSession_ReplaceItems(t *testing.T) {
	ctx := context.Background()
	sess, err := NewFileSession(t.TempDir(), "replace-1")
	if err != nil {
		t.Fatal(err)
	}

	old := agents.InputItemsFromText("old-1")
	old = append(old, agents.InputItemsFromText("old-2")...)
	old = append(old, agents.InputItemsFromText("old-3")...)
	if err := sess.AddItems(ctx, old); err != nil {
		t.Fatal(err)
	}

	repl := agents.InputItemsFromText("new-1")
	repl = append(repl, agents.InputItemsFromText("new-2")...)
	if err := sess.ReplaceItems(ctx, repl); err != nil {
		t.Fatal(err)
	}

	got, err := sess.GetItems(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after replace: got %d items, want 2", len(got))
	}
	for i, want := range repl {
		gb, _ := agents.MarshalInputItem(got[i])
		wb, _ := agents.MarshalInputItem(want)
		if string(gb) != string(wb) {
			t.Errorf("item %d: got %s, want %s", i, gb, wb)
		}
	}

	// No stale content may survive on disk — the file holds exactly the new lines.
	raw, err := os.ReadFile(sess.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "old-") {
		t.Errorf("old items still present in file:\n%s", raw)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != 2 {
		t.Errorf("file has %d lines, want 2", lines)
	}
}

func TestFileSession_ReplaceItemsEmptyClears(t *testing.T) {
	ctx := context.Background()
	sess, err := NewFileSession(t.TempDir(), "replace-empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddItems(ctx, agents.InputItemsFromText("hello")); err != nil {
		t.Fatal(err)
	}
	if err := sess.ReplaceItems(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got, err := sess.GetItems(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after empty replace: got %d items, want 0", len(got))
	}
}

func TestFileSession_ReplaceSessionItemsUsesAtomicPath(t *testing.T) {
	// The package-level assertion enforces this at compile time; keep a
	// behavioral check that the generic helper routes through it too.
	ctx := context.Background()
	sess, err := NewFileSession(t.TempDir(), "replace-helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(sess).(agents.ItemsReplacer); !ok {
		t.Fatal("FileSession must implement agents.ItemsReplacer")
	}
	if err := sess.AddItems(ctx, agents.InputItemsFromText("before")); err != nil {
		t.Fatal(err)
	}
	if err := agents.ReplaceSessionItems(ctx, sess, agents.InputItemsFromText("after")); err != nil {
		t.Fatal(err)
	}
	got, err := sess.GetItems(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after helper replace: got %d items, want 1", len(got))
	}
	b, _ := agents.MarshalInputItem(got[0])
	if !strings.Contains(string(b), "after") {
		t.Errorf("replaced item = %s, want the new content", b)
	}
}
