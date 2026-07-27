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
