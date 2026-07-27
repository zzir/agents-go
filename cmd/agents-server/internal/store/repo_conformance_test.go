package store_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
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
			Repo: store.NewSessionRepoAdapter(sessions, func(ref agents.SessionRef) agents.SessionStorage {
				return store.NewEntryStoreFor(db, ref)
			}),
		}
	})
}
