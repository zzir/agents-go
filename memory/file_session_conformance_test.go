package memory_test

import (
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
	"github.com/zzir/agents-go/memory"
)

func TestFileSessionConformance(t *testing.T) {
	agentstest.StorageConformance(t, func(t *testing.T) agents.SessionStorage {
		t.Helper()
		fs, err := memory.OpenFileSession(filepath.Join(t.TempDir(), "s.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return fs
	})
}
