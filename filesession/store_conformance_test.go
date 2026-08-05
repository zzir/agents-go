package filesession_test

import (
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agentstest"
	"github.com/zzir/agents-go/filesession"
)

func TestStoreConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(t *testing.T) session.Storage {
		t.Helper()
		store, err := filesession.NewAtPath(filepath.Join(t.TempDir(), "s.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}
