package store_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agentstest"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func TestEntryStoreConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(t *testing.T) session.Storage {
		t.Helper()
		db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := store.CreateSchema(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		return store.NewEntryStoreFor(db, session.Direct(store.NewID()))
	})
}
