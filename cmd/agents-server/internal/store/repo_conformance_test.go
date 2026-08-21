package store_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/internal/agentstest"
)

func TestServerRepoConformance(t *testing.T) {
	agentstest.RepoConformance(t, func(t *testing.T) agentstest.RepoUnderTest {
		t.Helper()
		db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := store.CreateSchema(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		sessions := store.NewSessionStore(db)
		return agentstest.RepoUnderTest{
			Repo: store.NewSessionRepoAdapter(sessions, func(ref session.Ref) session.Storage {
				return store.NewEntryStoreFor(db, ref)
			}),
		}
	})
}
