package session_test

import (
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

func TestInMemoryStorageConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(*testing.T) session.Storage {
		return session.NewInMemoryStorage("mem")
	})
}

// The in-process repo answers the same identity rules as the persistent ones.
// It ran neither conformance suite until now, which is how it came to be the
// one repo whose handle kept writing into a session that had been deleted.
func TestInMemoryRepoConformance(t *testing.T) {
	agentstest.RepoConformance(t, func(*testing.T) agentstest.RepoUnderTest {
		return agentstest.RepoUnderTest{Repo: session.NewInMemoryRepo()}
	})
}
