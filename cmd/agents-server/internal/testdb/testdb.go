// Package testdb opens the throwaway database the server's test suites share.
package testdb

import (
	"context"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// New opens an in-memory SQLite database with the schema created, closed when
// the test ends. Each call is its own database.
func New(tb testing.TB) *bun.DB {
	tb.Helper()
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
