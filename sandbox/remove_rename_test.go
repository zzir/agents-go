package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
)

// RemoveFile and Rename operate within the working directory: Rename creates
// the destination's parent dirs and moves the file, RemoveFile deletes it.
func TestLocalRemoveAndRename(t *testing.T) {
	ctx := context.Background()
	sb := &LocalSandbox{WorkDir: t.TempDir()}

	if err := sb.WriteFile(ctx, "a/x.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Rename into a not-yet-existing subdir: parent is created.
	if err := sb.Rename(ctx, "a/x.txt", "b/y.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := sb.ReadFile(ctx, "a/x.txt"); err == nil {
		t.Fatal("old path should be gone after rename")
	}
	if got, err := sb.ReadFile(ctx, "b/y.txt"); err != nil || string(got) != "hi" {
		t.Fatalf("read renamed: got %q err %v", got, err)
	}
	// Remove it; a second read fails.
	if err := sb.RemoveFile(ctx, "b/y.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := sb.ReadFile(ctx, "b/y.txt"); err == nil {
		t.Fatal("removed file should not read")
	}
}

// Without WorkDir the sandbox works in ONE temp directory — exec and the file
// operations share it across calls — and Close removes it.
func TestLocalTempDirIsSharedAndRemovedByClose(t *testing.T) {
	ctx := context.Background()
	sb := &LocalSandbox{}
	if err := sb.WriteFile(ctx, "note.txt", []byte("kept")); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := Exec(ctx, sb, ExecRequest{Cmd: []string{"sh", "-c", "cat note.txt && pwd"}})
	if err != nil || res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, "kept") {
		t.Fatalf("exec did not see the file the file op wrote: %+v, %v", res, err)
	}
	dir := sb.tmp
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp working directory %s survives Close: %v", dir, err)
	}
}
