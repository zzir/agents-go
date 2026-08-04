package docker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
// resolving every in-container path to an absolute one (leading "/").
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

// TestExclusiveCreateScripts_LeadingDashSafe locks the persistent-mode
// leading-dash defense: paths reach the script builder as absolute
// in-container paths (leading "/"), so mkdir/ln/rm can never parse one as an
// option. The persistent Docker path has no daemon-free integration test, so
// this asserts on the pure script builder directly.
func TestExclusiveCreateScripts_LeadingDashSafe(t *testing.T) {
	create, cleanup, rmTmp := exclusiveCreateScripts("/workspace/-f", "/workspace/.ap.dead", "aGk=")
	for name, script := range map[string]string{"create": create, "cleanup": cleanup, "rmTmp": rmTmp} {
		for tok := range strings.FieldsSeq(script) {
			if strings.HasPrefix(tok, "'-") {
				t.Errorf("%s: quoted argument starts with a dash: %q in %q", name, tok, script)
			}
		}
	}
	if !strings.Contains(create, "'/workspace/-f'") {
		t.Errorf("create: target not quoted absolute: %q", create)
	}
	if !strings.Contains(rmTmp, "'/workspace/.ap.dead'") {
		t.Errorf("rmTmp: tmp not quoted absolute: %q", rmTmp)
	}
}

// rootRel confines bind-mount file operations — which run on the HOST side of
// the mount — to the working directory: relative paths pass through for
// os.Root to police, absolute paths must lie under the in-container mount
// point (/workspace) and are translated, anything else is refused.
func TestRootRel(t *testing.T) {
	ok := map[string]string{
		"a/b":            filepath.FromSlash("a/b"),
		"../escape":      filepath.FromSlash("../escape"), // os.Root rejects it downstream
		"/workspace/a/b": filepath.FromSlash("a/b"),
		"/workspace":     ".",
		"":               ".",
	}
	for in, want := range ok {
		got, err := rootRel(in)
		if err != nil || got != want {
			t.Errorf("rootRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/", "/etc/passwd", "/workspacex/f"} {
		if got, err := rootRel(in); !errors.Is(err, sandbox.ErrOutsideWorkDir) {
			t.Errorf("rootRel(%q) = %q, %v; want ErrOutsideWorkDir", in, got, err)
		}
	}
}

// The model sees the working directory as /workspace (the in-container mount
// point) and echoes absolute paths from exec output back into the file tools;
// in bind-mount mode those must land on the same host file as their relative
// form. Host side only — no Docker daemon needed.
func TestBindMount_AbsolutePathsUnderMountPoint(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	s := &Sandbox{opts: Options{WorkDir: work}}

	if err := s.WriteFile(ctx, "/workspace/sub/a.txt", []byte("hi")); err != nil {
		t.Fatalf("WriteFile(/workspace/...): %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "sub", "a.txt")); err != nil || string(got) != "hi" {
		t.Fatalf("host file = %q, %v; want %q", got, err, "hi")
	}
	data, err := s.ReadFile(ctx, "/workspace/sub/a.txt")
	if err != nil || string(data) != "hi" {
		t.Fatalf("ReadFile(/workspace/...) = %q, %v; want %q", data, err, "hi")
	}
	if data, err = s.ReadFile(ctx, "sub/a.txt"); err != nil || string(data) != "hi" {
		t.Fatalf("ReadFile(relative) = %q, %v; want %q", data, err, "hi")
	}
	entries, err := s.ListDir(ctx, "/workspace/sub")
	if err != nil || len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("ListDir(/workspace/sub) = %v, %v; want one entry a.txt", entries, err)
	}

	// Absolute paths outside the mount point would land on the HOST filesystem,
	// which the container's isolation does not cover — refused, not re-rooted.
	if _, err := s.ReadFile(ctx, "/etc/passwd"); !errors.Is(err, sandbox.ErrOutsideWorkDir) {
		t.Fatalf("ReadFile(/etc/passwd) = %v; want ErrOutsideWorkDir", err)
	}
	if err := s.WriteFile(ctx, "/tmp/x", []byte("x")); !errors.Is(err, sandbox.ErrOutsideWorkDir) {
		t.Fatalf("WriteFile(/tmp/x) = %v; want ErrOutsideWorkDir", err)
	}
}
