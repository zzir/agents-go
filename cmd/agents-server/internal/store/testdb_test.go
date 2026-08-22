package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// newTestDB opens a fresh database with the schema applied: the PostgreSQL
// server named by AGENTS_PG_TEST_DSN when set (a throwaway schema per test,
// so the whole suite runs on both dialects in CI), an in-memory SQLite
// otherwise.
//
//	docker run -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=agents_test -p 54329:5432 postgres:16-alpine
//	AGENTS_PG_TEST_DSN='postgres://postgres:test@localhost:54329/agents_test?sslmode=disable' go test ./internal/store/
func newTestDB(t *testing.T) *bun.DB {
	t.Helper()
	if os.Getenv("AGENTS_PG_TEST_DSN") != "" {
		return pgTestDB(t)
	}
	db, err := NewSQLiteDB("file:" + NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := CreateSchema(context.Background(), db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

// pgTestDB is newTestDB's PostgreSQL half, for the tests that exist only to
// prove a PostgreSQL-specific branch; it skips when no server is named.
func pgTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("AGENTS_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("AGENTS_PG_TEST_DSN not set; skipping PostgreSQL store tests")
	}
	ctx := context.Background()
	schema := "t_" + strings.ReplaceAll(NewID(), "-", "") // a UUID's hyphens are not identifier characters

	admin := NewPostgresDB(dsn)
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
	if err := CreateSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

// ids returns a per-test memo: the same name yields the same fresh UUID,
// so a test can keep naming things "s1"/"r1" while the rows carry real ids.
func ids(t *testing.T) func(name string) string {
	t.Helper()
	var mu sync.Mutex
	memo := map[string]string{}
	return func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		if id, ok := memo[name]; ok {
			return id
		}
		id := NewID()
		memo[name] = id
		return id
	}
}
