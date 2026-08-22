package store_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/internal/agentstest"
)

// pgTestDB opens the PostgreSQL server named by AGENTS_PG_TEST_DSN (skipping
// the test when unset) with a fresh throwaway schema, so each test sees empty
// tables and leaves nothing behind.
//
//	docker run -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=agents_test -p 54329:5432 postgres:16-alpine
//	AGENTS_PG_TEST_DSN='postgres://postgres:test@localhost:54329/agents_test?sslmode=disable' go test ./internal/store/
func pgTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("AGENTS_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("AGENTS_PG_TEST_DSN not set; skipping PostgreSQL store tests")
	}
	ctx := context.Background()
	schema := "t_" + strings.ReplaceAll(store.NewID(), "-", "") // a UUID's hyphens are not identifier characters

	admin := store.NewPostgresDB(dsn)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("creating test schema (is the server up?): %v", err)
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dsn),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}),
	))
	db := bun.NewDB(sqldb, pgdialect.New())
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})
	if err := store.CreateSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPGEntryStoreConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(t *testing.T) session.Storage {
		t.Helper()
		return store.NewEntryStoreFor(pgTestDB(t), session.Direct(store.NewID()))
	})
}

func TestPGRepoConformance(t *testing.T) {
	agentstest.RepoConformance(t, func(t *testing.T) agentstest.RepoUnderTest {
		t.Helper()
		db := pgTestDB(t)
		sessions := store.NewSessionStore(db)
		// Every id column is uuid-typed on PostgreSQL: the suite's literal
		// names become memoized UUIDs.
		ids := map[string]string{}
		return agentstest.RepoUnderTest{
			Repo: store.NewSessionRepoAdapter(sessions, func(ref session.Ref) session.Storage {
				return store.NewEntryStoreFor(db, ref)
			}),
			IDs: func(name string) string {
				if id, ok := ids[name]; ok {
					return id
				}
				ids[name] = store.NewID()
				return ids[name]
			},
		}
	})
}

// A duplicate insert must map to a 409-able UniqueViolation on PostgreSQL just
// as it does on SQLite, with the offending column named.
func TestPGUniqueViolation(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	agents := store.NewAgentConfigStore(db)
	if err := agents.Create(ctx, &store.AgentConfig{Name: "dup"}); err != nil {
		t.Fatal(err)
	}
	err := agents.Create(ctx, &store.AgentConfig{Name: "dup"})
	if err == nil {
		t.Fatal("duplicate agent name inserted without error")
	}
	cols, ok := store.UniqueViolation(err)
	if !ok || !strings.Contains(cols, "name") {
		t.Fatalf("UniqueViolation(%v) = %q, %v; want the name column reported", err, cols, ok)
	}
}

// The summary listing's JSON surgery is dialect-specific SQL — prove the
// PostgreSQL branch strips payload fields, flags the row, and leaves the full
// row reachable by span.
func TestPGTraceSummary(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	traces := store.NewTraceStore(db)
	sessionID, runID := store.NewID(), store.NewID()
	ev := &store.TraceEvent{
		SessionID: sessionID, RunID: runID, Kind: "span", SpanID: "sp1", Name: "generation",
		Data:      `{"model":"gpt","input":[{"role":"user"}],"output":"big"}`,
		CreatedAt: time.Now().UTC(),
	}
	if err := traces.Insert(ctx, ev); err != nil {
		t.Fatal(err)
	}
	rows, err := traces.ListSummaryBySession(ctx, sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].PayloadOmitted {
		t.Error("PayloadOmitted = false, want true for a row with payload fields")
	}
	if strings.Contains(rows[0].Data, "input") || !strings.Contains(rows[0].Data, "model") {
		t.Errorf("summary data = %q, want payload stripped and the rest kept", rows[0].Data)
	}
	full, err := traces.GetBySpan(ctx, sessionID, "sp1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Data, "input") {
		t.Errorf("GetBySpan data = %q, want the payload intact", full.Data)
	}
}

// Two workflows whose names differ only by case must collide on PostgreSQL
// (lower(name) unique index) as they do on SQLite (COLLATE NOCASE).
func TestPGWorkflowNameCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	workflows := store.NewWorkflowStore(db)
	if err := workflows.Create(ctx, &store.Workflow{Name: "Build"}); err != nil {
		t.Fatal(err)
	}
	err := workflows.Create(ctx, &store.Workflow{Name: "build"})
	if err == nil {
		t.Fatal("case-variant workflow name inserted without error")
	}
	if _, ok := store.UniqueViolation(err); !ok {
		t.Fatalf("want a UniqueViolation, got %v", err)
	}
}
