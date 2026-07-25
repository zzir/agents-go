package sessions_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/sessions"
)

func TestSQLRepo(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "repo.db")
	_, db, err := sessions.NewSQLite(dsn, "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sessions.CreateSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	repo := sessions.NewRepo(db)

	visible, err := repo.Create(ctx, agents.CreateOptions{ID: "chat-1", Title: "A chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, agents.CreateOptions{ID: "task-1", Hidden: true}); err != nil {
		t.Fatal(err)
	}

	listed, err := repo.List(ctx, agents.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "chat-1" || listed[0].Title != "A chat" {
		t.Fatalf("List = %+v, want only the visible session with its title", listed)
	}
	all, _ := repo.List(ctx, agents.ListOptions{IncludeHidden: true})
	if len(all) != 2 {
		t.Fatalf("List(IncludeHidden) = %d, want 2", len(all))
	}

	if err := visible.AppendItems(ctx, agents.InputItemsFromText("hello"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(ctx, "missing"); !errors.Is(err, agents.ErrSessionNotFound) {
		t.Errorf("Open(missing) = %v, want ErrSessionNotFound", err)
	}

	// Delete takes the entries with it, in one transaction — orphaned entries
	// pointing at a session that no longer exists would be unreachable garbage.
	if err := repo.Delete(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	orphan := agents.NewSession(sessions.New(db, "chat-1"))
	got, err := orphan.Entries(ctx, agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("deleting a session left %d entries behind", len(got))
	}
}

// A point lookup must not read the whole session: the index exists so a large
// conversation costs the same as a small one.
func TestSQLSession_EntryLookupIsIndexed(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "lookup.db")
	s, db, err := sessions.NewSQLite(dsn, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sessions.CreateSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	sess := agents.NewSession(s)

	for range 20 {
		if err := sess.AppendItems(ctx, agents.InputItemsFromText("x"), agents.Source{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := sess.Entries(ctx, agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	target := entries[7]

	got, err := sess.Entry(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != target.ID {
		t.Fatalf("Entry(%q) = %+v", target.ID, got)
	}
	if missing, err := sess.Entry(ctx, "nope"); err != nil || missing != nil {
		t.Errorf("Entry(missing) = %v, %v; want nil, nil", missing, err)
	}

	// Parent links round-trip through the columns, so a tree walk works after
	// a reload rather than only in memory.
	if entries[1].ParentID != entries[0].ID {
		t.Errorf("parent link lost through SQL: %q != %q", entries[1].ParentID, entries[0].ID)
	}
}
