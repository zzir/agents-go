package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// TestFileSession_WriteLinesCleansTempOnRenameFailure ensures the atomic-rewrite
// path does not leak a .session-* temp file when the final os.Rename fails.
//
// The failure is injected by making the session path an existing directory:
// renaming a freshly written temp file onto a directory fails with EISDIR on
// Unix, exercising the rename-error branch without unusual filesystem tricks.
func TestFileSession_WriteLinesCleansTempOnRenameFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Occupy the session path with a directory so os.Rename(tmp, path) fails.
	sessionPath := filepath.Join(dir, "session.jsonl")
	if err := os.Mkdir(sessionPath, 0o755); err != nil {
		t.Fatal(err)
	}

	sess, err := OpenFileSession(sessionPath)
	if err != nil {
		t.Fatal(err)
	}

	// ReplaceEntries routes through writeLines; the rename at its tail must fail.
	if err := agents.ReplaceStorageEntries(ctx, sess, mustEntries(t, agents.InputItemsFromText("hello"))...); err == nil {
		t.Fatal("ReplaceEntries succeeded, but the rename onto a directory should fail")
	}

	// The temp file is created in filepath.Dir(sessionPath) == dir; none may
	// survive the failed rename.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".session-") {
			t.Errorf("leaked temp file after rename failure: %s", e.Name())
		}
	}
}
