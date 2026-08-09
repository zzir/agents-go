package store

import (
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/agents/tasks/conformancetest"
)

// The server's TaskStore (through its adapter, the form the Manager consumes)
// passes the same behavioral contract as the SDK's in-memory store and the
// sessions module's — one suite, three implementations, no drift. The
// session-generation predicates are server-specific behavior on TOP of the
// contract and keep their own tests.
func TestTaskAdapterConformance(t *testing.T) {
	conformancetest.Run(t, func(t *testing.T) tasks.Store {
		t.Helper()
		return NewTaskAdapter(NewTaskStore(newTestDB(t)))
	})
}
