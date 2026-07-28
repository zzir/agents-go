package agents_test

import (
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
)

func TestInMemoryStorageConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(*testing.T) agents.SessionStorage {
		return agents.NewInMemoryStorage("mem")
	})
}

// The in-process repo answers the same identity rules as the persistent ones.
// It ran neither conformance suite until now, which is how it came to be the
// one repo whose handle kept writing into a session that had been deleted.
func TestInMemoryRepoConformance(t *testing.T) {
	agentstest.RepoConformance(t, func(*testing.T) agentstest.RepoUnderTest {
		return agentstest.RepoUnderTest{Repo: agents.NewInMemoryRepo()}
	})
}
