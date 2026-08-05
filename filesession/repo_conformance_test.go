package filesession_test

import (
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agentstest"
	"github.com/zzir/agents-go/filesession"
)

func TestFileRepoConformance(t *testing.T) {
	agentstest.RepoConformance(t, func(t *testing.T) agentstest.RepoUnderTest {
		t.Helper()
		dir := t.TempDir()
		repo, err := filesession.NewRepo(dir)
		if err != nil {
			t.Fatal(err)
		}
		return agentstest.RepoUnderTest{
			Repo: repo,
			Direct: func(id string) (*session.Session, error) {
				store, err := filesession.New(dir, id)
				if err != nil {
					return nil, err
				}
				return session.NewSession(store), nil
			},
		}
	})
}
