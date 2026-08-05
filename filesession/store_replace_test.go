package filesession

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

func TestStore_ReplaceEntries(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir(), "replace-1")
	if err != nil {
		t.Fatal(err)
	}

	old := agents.InputItemsFromText("old-1")
	old = append(old, agents.InputItemsFromText("old-2")...)
	old = append(old, agents.InputItemsFromText("old-3")...)
	if err := session.NewSession(store).AppendItems(ctx, old, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	repl := agents.InputItemsFromText("new-1")
	repl = append(repl, agents.InputItemsFromText("new-2")...)
	if err := session.ReplaceEntries(ctx, store, mustEntries(t, repl)...); err != nil {
		t.Fatal(err)
	}

	got, err := session.NewSession(store).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after replace: got %d items, want 2", len(got))
	}
	for i, want := range repl {
		gb, _ := session.MarshalInputItem(got[i])
		wb, _ := session.MarshalInputItem(want)
		if string(gb) != string(wb) {
			t.Errorf("item %d: got %s, want %s", i, gb, wb)
		}
	}

	// No stale content may survive on disk — the file holds exactly the new lines.
	raw, err := os.ReadFile(store.path)
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

func TestStore_ReplaceEntriesEmptyClears(t *testing.T) {
	ctx := context.Background()
	store, err := New(t.TempDir(), "replace-empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.NewSession(store).AppendItems(ctx, agents.InputItemsFromText("hello"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	if err := session.ReplaceEntries(ctx, store, mustEntries(t, nil)...); err != nil {
		t.Fatal(err)
	}
	got, err := session.NewSession(store).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after empty replace: got %d items, want 0", len(got))
	}
}

func TestStore_ReplaceSessionItemsUsesAtomicPath(t *testing.T) {
	// The package-level assertion enforces this at compile time; keep a
	// behavioral check that the generic helper routes through it too.
	ctx := context.Background()
	store, err := New(t.TempDir(), "replace-helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(store).(session.AtomicReplacer); !ok {
		t.Fatal("Store must implement session.AtomicReplacer")
	}
	if err := session.NewSession(store).AppendItems(ctx, agents.InputItemsFromText("before"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	if err := session.ReplaceEntries(ctx, store, mustEntries(t, agents.InputItemsFromText("after"))...); err != nil {
		t.Fatal(err)
	}
	got, err := session.NewSession(store).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after helper replace: got %d items, want 1", len(got))
	}
	b, _ := session.MarshalInputItem(got[0])
	if !strings.Contains(string(b), "after") {
		t.Errorf("replaced item = %s, want the new content", b)
	}
}
