package sessions_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/sessions"
)

func item(text string) agents.TResponseInputItem {
	return agents.InputItemsFromText(text)[0]
}

func jsonOf(t *testing.T, it agents.TResponseInputItem) string {
	t.Helper()
	b, err := agents.MarshalInputItem(it)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runSessionContract exercises the full Session contract against any backend.
func runSessionContract(t *testing.T, s *sessions.Session) {
	t.Helper()
	ctx := context.Background()

	got, err := agents.SessionItems(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("new session: got %d items, want 0", len(got))
	}

	in := []agents.TResponseInputItem{item("a"), item("b"), item("c")}
	if err := agents.AddSessionItems(ctx, s, in, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	got, err = agents.SessionItems(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	for i := range in {
		if jsonOf(t, got[i]) != jsonOf(t, in[i]) {
			t.Errorf("item %d: got %s, want %s", i, jsonOf(t, got[i]), jsonOf(t, in[i]))
		}
	}

	// Most recent 2, still oldest-first => b, c.
	got, err = agents.SessionItems(ctx, s, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || jsonOf(t, got[0]) != jsonOf(t, in[1]) || jsonOf(t, got[1]) != jsonOf(t, in[2]) {
		t.Errorf("limit=2 returned wrong items: %v", got)
	}

	// Pop returns the most recent (c) and shrinks the history.
	last, err := s.PopEntry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last == nil {
		t.Fatal("pop returned nothing")
	}
	lastItem, err := last.InputItem()
	if err != nil {
		t.Fatal(err)
	}
	if jsonOf(t, lastItem) != jsonOf(t, in[2]) {
		t.Errorf("pop: got %v, want c", lastItem)
	}
	got, _ = agents.SessionItems(ctx, s, 0)
	if len(got) != 2 {
		t.Errorf("after pop: got %d items, want 2", len(got))
	}

	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = agents.SessionItems(ctx, s, 0)
	if len(got) != 0 {
		t.Errorf("after clear: got %d items, want 0", len(got))
	}

	last, err = s.PopEntry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last != nil {
		t.Errorf("pop on empty: got %v, want nil", last)
	}
}

func TestSQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "agents.db")
	s, db, err := sessions.NewSQLite(dsn, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sessions.CreateSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	runSessionContract(t, s)
}

func TestSQLite_SessionIsolation(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "agents.db")
	a, db, err := sessions.NewSQLite(dsn, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sessions.CreateSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	b := sessions.New(db, "b")

	if err := agents.AddSessionItems(ctx, a, []agents.TResponseInputItem{item("only-a")}, agents.Source{}); err != nil {
		t.Fatal(err)
	}
	got, _ := agents.SessionItems(ctx, b, 0)
	if len(got) != 0 {
		t.Errorf("session b leaked %d items from a", len(got))
	}
}

// TestPostgres runs the full contract against a real PostgreSQL instance when
// AGENTS_TEST_PG_DSN is set (e.g. "postgres://user:pass@localhost:5432/db?sslmode=disable");
// it is skipped otherwise.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("AGENTS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AGENTS_TEST_PG_DSN to run PostgreSQL session tests")
	}
	ctx := context.Background()
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer sqldb.Close()

	s, bdb := sessions.NewPostgres(sqldb, "sess-pg")
	if err := sessions.CreateSchema(ctx, bdb); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ctx); err != nil { // drop leftovers from a prior run
		t.Fatal(err)
	}
	runSessionContract(t, s)
}

// mustEntries wraps plain items as item entries for tests exercising replace.
func mustEntries(t *testing.T, items []agents.TResponseInputItem) []agents.SessionEntry {
	t.Helper()
	entries, err := agents.NewItemEntries(items, agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
