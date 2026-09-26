// Package testdb opens the throwaway database the server's test suites share.
package testdb

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// New opens a fresh database with the schema created, closed when the test
// ends: a throwaway schema on the PostgreSQL server AGENTS_PG_TEST_DSN names
// when set, else an in-memory SQLite. Each call is its own database.
func New(tb testing.TB) *bun.DB {
	tb.Helper()
	if dsn := os.Getenv("AGENTS_PG_TEST_DSN"); dsn != "" {
		return NewPostgres(tb, dsn)
	}
	db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
	if err != nil {
		tb.Fatalf("open db: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	if err := store.CreateSchema(context.Background(), db); err != nil {
		tb.Fatalf("schema: %v", err)
	}
	return db
}

// NewPostgres opens a schema of its own on the server at dsn, created with
// the tables and dropped when the test ends. The store package's own tests
// carry a copy (they cannot import this package).
func NewPostgres(tb testing.TB, dsn string) *bun.DB {
	tb.Helper()
	ctx := context.Background()
	schema := "t_" + strings.ReplaceAll(store.NewID(), "-", "") // a UUID's hyphens are not identifier characters

	admin := store.NewPostgresDB(dsn)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		tb.Fatalf("creating test schema (is the server up?): %v", err)
	}
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dsn),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}),
	))
	db := bun.NewDB(sqldb, pgdialect.New())
	tb.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})
	if err := store.CreateSchema(ctx, db); err != nil {
		tb.Fatal(err)
	}
	return db
}

// SkipOnPostgres skips a test whose fixture PostgreSQL refuses — literal ids
// in uuid columns, or a 404 that a malformed id turns into a 400 there — so
// it keeps its SQLite coverage while the suite runs on both.
func SkipOnPostgres(tb testing.TB) {
	tb.Helper()
	if os.Getenv("AGENTS_PG_TEST_DSN") != "" {
		tb.Skip("fixture is SQLite-shaped (literal ids); skipped on PostgreSQL")
	}
}
