package sessions_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/agents/tasks/conformancetest"
	"github.com/zzir/agents-go/sessions"
)

// The sessions module's TaskStore passes the same behavioral contract as the
// SDK's in-memory store and the server's — one suite, three implementations,
// no drift. The session-generation predicates are this store's own behavior
// on TOP of the contract and keep their tests in tasks_generation_test.go.
func TestSQLTaskStoreConformance(t *testing.T) {
	conformancetest.Run(t, func(t *testing.T) tasks.Store {
		t.Helper()
		dsn := "file:" + filepath.Join(t.TempDir(), "tasks.db")
		_, db, err := sessions.NewSQLite(dsn, "unused")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := sessions.CreateTaskSchema(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		return sessions.NewTaskStore(db)
	})
}
