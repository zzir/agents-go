package main

import (
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/session/sessiontest"
)

// A session backend of your own runs the SDK's conformance suites from its
// tests: StorageConformance for the storage, RepoConformance for the repo
// that hands sessions out. The in-memory pair stands in for yours here.
func TestMyStorageConformance(t *testing.T) {
	sessiontest.StorageConformance(t, func(*testing.T) session.Storage {
		return session.NewInMemoryStorage("mine")
	})
}

func TestMyRepoConformance(t *testing.T) {
	sessiontest.RepoConformance(t, func(*testing.T) sessiontest.RepoUnderTest {
		return sessiontest.RepoUnderTest{Repo: session.NewInMemoryRepo()}
	})
}
