package sandbox

import (
	"context"
	"path/filepath"
	"testing"
)

// The file tools share exec's path view (shell semantics): a relative path
// resolves under WorkDir, an absolute path is used as-is. The model learns
// real absolute paths from exec output (pwd, ls, git status) and echoes them
// back into the file tools — both spellings must reach the same file.
func TestLocalFilePathsShellSemantics(t *testing.T) {
	ctx := context.Background()
	wd := t.TempDir()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: wd})

	abs := func(parts ...string) string {
		return filepath.Join(append([]string{wd}, parts...)...)
	}

	if err := sb.WriteFile(ctx, abs("sub", "a.txt"), []byte("hi")); err != nil {
		t.Fatalf("WriteFile(absolute): %v", err)
	}
	got, err := sb.ReadFile(ctx, "sub/a.txt")
	if err != nil || string(got) != "hi" {
		t.Fatalf("ReadFile(relative) = %q, %v; want %q", got, err, "hi")
	}
	got, err = sb.ReadFile(ctx, abs("sub", "a.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("ReadFile(absolute) = %q, %v; want %q", got, err, "hi")
	}

	// ".." resolves like a shell would.
	got, err = sb.ReadFile(ctx, "sub/../sub/a.txt")
	if err != nil || string(got) != "hi" {
		t.Fatalf("ReadFile(with ..) = %q, %v; want %q", got, err, "hi")
	}

	entries, err := sb.ListDir(ctx, abs("sub"))
	if err != nil || len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("ListDir(absolute) = %v, %v; want one entry a.txt", entries, err)
	}

	if err := sb.Rename(ctx, abs("sub", "a.txt"), abs("sub", "b.txt")); err != nil {
		t.Fatalf("Rename(absolute): %v", err)
	}
	if err := sb.RemoveFile(ctx, abs("sub", "b.txt")); err != nil {
		t.Fatalf("RemoveFile(absolute): %v", err)
	}

	// An absolute path outside WorkDir reaches the same real filesystem exec
	// sees — the file tools are not a second, narrower view.
	outside := t.TempDir()
	if err := sb.WriteFile(ctx, filepath.Join(outside, "real.txt"), []byte("real fs")); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}
	got, err = sb.ReadFile(ctx, filepath.Join(outside, "real.txt"))
	if err != nil || string(got) != "real fs" {
		t.Fatalf("ReadFile(outside) = %q, %v; want %q", got, err, "real fs")
	}
}
