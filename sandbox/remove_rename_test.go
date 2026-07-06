package sandbox

import (
	"context"
	"errors"
	"testing"
)

// RemoveFile and Rename operate within the working directory: Rename creates
// the destination's parent dirs and moves the file, RemoveFile deletes it, and
// both report ErrNoWorkDir when no working directory is configured.
func TestLocalRemoveAndRename(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})

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

	// Without a working directory both ops are ErrNoWorkDir, not a silent no-op.
	bare := NewLocal()
	if err := bare.RemoveFile(ctx, "x"); !errors.Is(err, ErrNoWorkDir) {
		t.Fatalf("RemoveFile no workdir: err = %v, want ErrNoWorkDir", err)
	}
	if err := bare.Rename(ctx, "x", "y"); !errors.Is(err, ErrNoWorkDir) {
		t.Fatalf("Rename no workdir: err = %v, want ErrNoWorkDir", err)
	}
}
