package sessions_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agentstest"
	"github.com/zzir/agents-go/sessions"
)

func TestSQLSessionConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(t *testing.T) session.Storage {
		t.Helper()
		_, db, err := sessions.NewSQLite("file:"+filepath.Join(t.TempDir(), "c.db"), "unused")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if err := sessions.CreateSchema(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		return sessions.New(db, "s1")
	})
}
