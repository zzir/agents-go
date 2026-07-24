package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// the local sandbox confines file operations to WorkDir via os.Root. A
// symlink created under WorkDir that points outside must not be followed to
// read or write the external target, and a symlinked subdirectory must not be
// traversed out of the workdir.
func TestLocalSandbox_SymlinkEscapeRejected(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(work, "escape")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	sb := NewLocalWithOptions(LocalOptions{WorkDir: work})

	if data, err := sb.ReadFile(ctx, "escape"); err == nil {
		t.Fatalf("ReadFile followed the symlink out of the workdir: got %q", data)
	}
	if err := sb.WriteFile(ctx, "escape", []byte("x")); err == nil {
		t.Fatal("WriteFile followed the symlink out of the workdir")
	}
	if got, _ := os.ReadFile(secret); string(got) != "TOP SECRET" {
		t.Fatalf("external file was modified through the symlink: %q", got)
	}

	if err := os.Symlink(outside, filepath.Join(work, "extdir")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := sb.ReadFile(ctx, "extdir/secret.txt"); err == nil {
		t.Fatal("ReadFile traversed a symlinked directory out of the workdir")
	}
	if _, err := sb.ListDir(ctx, "extdir"); err == nil {
		t.Fatal("ListDir traversed a symlinked directory out of the workdir")
	}
}
