package sessions_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/session/sessiontest"
	"github.com/zzir/agents-go/sessions"
)

func TestSQLRepoConformance(t *testing.T) {
	sessiontest.RepoConformance(t, func(t *testing.T) sessiontest.RepoUnderTest {
		t.Helper()
		_, db, err := sessions.NewSQLite("file:"+filepath.Join(t.TempDir(), "r.db"), "unused")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := sessions.CreateSchema(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		return sessiontest.RepoUnderTest{
			Repo: sessions.NewRepo(db),
			Direct: func(id string) (*session.Session, error) {
				return session.NewSession(sessions.New(db, id)), nil
			},
		}
	})
}
