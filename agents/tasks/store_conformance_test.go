package tasks_test

import (
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/agents/tasks/conformancetest"
)

// The in-memory store passes the same behavioral contract as the SQL stores —
// one suite, three implementations, no drift.
func TestInMemoryStoreConformance(t *testing.T) {
	conformancetest.Run(t, func(t *testing.T) tasks.Store {
		t.Helper()
		return tasks.NewInMemoryStore()
	})
}
