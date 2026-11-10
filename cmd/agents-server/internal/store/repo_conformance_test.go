package store_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/session/sessiontest"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func TestServerRepoConformance(t *testing.T) {
	sessiontest.RepoConformance(t, func(t *testing.T) sessiontest.RepoUnderTest {
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
		// A session id is a UUID here: the suite's literal names become memoized ones.
		ids := map[string]string{}
		return sessiontest.RepoUnderTest{
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
