package docker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzir/agents-go/sandbox"
)

// in bind-mount mode the docker sandbox performs file operations on the
// host WorkDir; they must be confined to it via os.Root. A symlink created
// inside the container (i.e. under the bind-mounted host directory) that points
// outside must not be followed to read or write host files. This exercises the
// host side directly, so it needs no Docker daemon.
func TestBindMount_SymlinkEscapeRejected(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("HOST SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(work, "escape")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	s := &Sandbox{opts: Options{WorkDir: work}}

	if data, err := s.ReadFile(ctx, "escape"); err == nil {
		t.Fatalf("ReadFile followed the symlink onto the host: got %q", data)
	}
	if err := s.WriteFile(ctx, "escape", []byte("x")); err == nil {
		t.Fatal("WriteFile followed the symlink onto the host")
	}
	if got, _ := os.ReadFile(secret); string(got) != "HOST SECRET" {
		t.Fatalf("host file was modified through the symlink: %q", got)
	}

	if err := os.Symlink(outside, filepath.Join(work, "extdir")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := s.ReadFile(ctx, "extdir/secret.txt"); err == nil {
		t.Fatal("ReadFile traversed a symlinked directory onto the host")
	}
	if _, err := s.ListDir(ctx, "extdir"); err == nil {
		t.Fatal("ListDir traversed a symlinked directory onto the host")
	}
}

// A normal (non-escaping) bind-mount round-trip still works after the os.Root
// change: write into a nested dir, read it back, list it, rename and remove.
func TestBindMount_NormalOps(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	s := &Sandbox{opts: Options{WorkDir: work}}

	if err := s.WriteFile(ctx, "sub/a.txt", []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := s.ReadFile(ctx, "sub/a.txt"); err != nil || string(got) != "hello" {
		t.Fatalf("read: got %q err %v", got, err)
	}
	entries, err := s.ListDir(ctx, "sub")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("list = %+v, want a single a.txt", entries)
	}
	if err := s.Rename(ctx, "sub/a.txt", "b.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := s.ReadFile(ctx, "b.txt"); err != nil {
		t.Fatalf("read after rename: %v", err)
	}
	if err := s.RemoveFile(ctx, "b.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := s.ReadFile(ctx, "b.txt"); err == nil {
		t.Fatal("removed file should not read back")
	}
}

// persistent-mode ListDir parses the NUL-separated output of "find". A
// filename containing a tab or a newline must not corrupt the listing (the name
// is the final \t-field, and records are NUL-separated).
func TestParseFindEntries(t *testing.T) {
	out := "d\t4096\tnormaldir\x00" +
		"f\t10\tweird\tname\x00" + // tab inside the filename
		"f\t20\thas\nnewline\x00" + // newline inside the filename
		"f\t5\tplain\x00"
	got := parseFindEntries(out)
	want := []sandbox.DirEntry{
		{Name: "normaldir", IsDir: true, Size: 4096},
		{Name: "weird\tname", IsDir: false, Size: 10},
		{Name: "has\nnewline", IsDir: false, Size: 20},
		{Name: "plain", IsDir: false, Size: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Empty output (an empty directory) yields no entries, not a phantom one from
// the trailing NUL.
func TestParseFindEntries_Empty(t *testing.T) {
	if got := parseFindEntries(""); len(got) != 0 {
		t.Errorf("empty output = %+v, want none", got)
	}
}

// TestBindMount_LeadingDashName verifies a filename starting with "-" is treated
// as a path, not an option: CreateExclusive works on "-f" in bind-mount mode
// (os.Root uses it as a name). The persistent-shell path guards the same case by
// prefixing every in-container path with "./".
func TestBindMount_LeadingDashName(t *testing.T) {
	work := t.TempDir()
	s := &Sandbox{opts: Options{WorkDir: work}}
	ctx := context.Background()
	if err := s.CreateExclusive(ctx, "-f", []byte("hi")); err != nil {
		t.Fatalf("CreateExclusive -f: %v", err)
	}
	got, err := s.ReadFile(ctx, "-f")
	if err != nil {
		t.Fatalf("ReadFile -f: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("content = %q, want hi", got)
	}
	if err := s.CreateExclusive(ctx, "-f", []byte("x")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: want fs.ErrExist, got %v", err)
	}
}
