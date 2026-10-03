package session_test

import (
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/session/sessiontest"
)

func TestInMemoryStorageConformance(t *testing.T) {
	sessiontest.StorageConformance(t, func(*testing.T) session.Storage {
		return session.NewInMemoryStorage("mem")
	})
}

// The in-process repo answers the same identity rules as the persistent ones.
// It ran neither conformance suite until now, which is how it came to be the
// one repo whose handle kept writing into a session that had been deleted.
func TestInMemoryRepoConformance(t *testing.T) {
	sessiontest.RepoConformance(t, func(*testing.T) sessiontest.RepoUnderTest {
		return sessiontest.RepoUnderTest{Repo: session.NewInMemoryRepo()}
	})
}
