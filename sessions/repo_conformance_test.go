package sessions_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
	"github.com/zzir/agents-go/sessions"
)

func TestSQLRepoConformance(t *testing.T) {
	agentstest.RepoConformance(t, func(t *testing.T) agentstest.RepoUnderTest {
		t.Helper()
		_, db, err := sessions.NewSQLite("file:"+filepath.Join(t.TempDir(), "r.db"), "unused")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := sessions.CreateSchema(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		return agentstest.RepoUnderTest{
			Repo: sessions.NewRepo(db),
			Direct: func(id string) (*agents.Session, error) {
				return agents.NewSession(sessions.New(db, id)), nil
			},
		}
	})
}
