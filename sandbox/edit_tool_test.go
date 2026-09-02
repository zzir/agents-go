package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seed(t *testing.T, sb Sandbox, path, content string) {
	t.Helper()
	if err := sb.WriteFile(context.Background(), path, []byte(content)); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func mustRead(t *testing.T, sb Sandbox, path string) string {
	t.Helper()
	data, err := sb.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// A patch that updates, adds, and renames applies end-to-end against a real
// (local) sandbox.
func TestApplyPatchEndToEnd(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	seed(t, sb, "main.go", "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n")
	seed(t, sb, "old.go", "package old\n")

	patch := "*** Begin Patch\n" +
		"*** Update File: main.go\n" +
		" func main() {\n" +
		"-\tprintln(\"hi\")\n" +
		"+\tprintln(\"bye\")\n" +
		"*** Add File: extra.txt\n" +
		"+hello\n" +
		"*** Update File: old.go\n" +
		"*** Move to: new.go\n" +
		" package old\n" +
		"+// moved\n" +
		"*** End Patch\n"

	out, err := applyPatch(ctx, sb, patch)
	if err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if mustRead(t, sb, "main.go") != "package main\n\nfunc main() {\n\tprintln(\"bye\")\n}\n" {
		t.Fatalf("main.go not updated:\n%s", mustRead(t, sb, "main.go"))
	}
	if mustRead(t, sb, "extra.txt") != "hello" {
		t.Fatalf("extra.txt = %q", mustRead(t, sb, "extra.txt"))
	}
	if mustRead(t, sb, "new.go") != "package old\n// moved\n" {
		t.Fatalf("new.go = %q", mustRead(t, sb, "new.go"))
	}
	if _, err := sb.ReadFile(ctx, "old.go"); err == nil {
		t.Fatal("old.go should be gone after move")
	}
	t.Logf("summary:\n%s", out)
}

// A "Move to:" naming the section's own path is a plain update, not a
// duplicate-section conflict.
func TestApplyPatchDegenerateMoveIsPlainUpdate(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	seed(t, sb, "f.go", "old\n")

	patch := "*** Begin Patch\n" +
		"*** Update File: f.go\n" +
		"*** Move to: f.go\n" +
		"-old\n" +
		"+new\n" +
		"*** End Patch\n"
	if _, err := applyPatch(ctx, sb, patch); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}
	if got := mustRead(t, sb, "f.go"); got != "new\n" {
		t.Errorf("f.go = %q, want %q", got, "new\n")
	}
}

// Validation atomicity: a patch whose later hunk can't be located changes
// nothing — the earlier Add is never committed.
func TestApplyPatchValidationAtomic(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	seed(t, sb, "exist.txt", "real content\n")

	patch := "*** Begin Patch\n" +
		"*** Add File: new.txt\n" +
		"+hello\n" +
		"*** Update File: exist.txt\n" +
		" nomatch\n" +
		"-x\n" +
		"+y\n" +
		"*** End Patch\n"

	if _, err := applyPatch(ctx, sb, patch); err == nil {
		t.Fatal("expected failure: second hunk context not found")
	}
	if _, err := sb.ReadFile(ctx, "new.txt"); err == nil {
		t.Fatal("new.txt must not exist — plan failed before any write")
	}
	if mustRead(t, sb, "exist.txt") != "real content\n" {
		t.Fatal("exist.txt must be untouched")
	}
}

// failingSandbox fails the Nth CreateExclusive (or WriteFile) so commit-phase
// rollback can be tested. Add/Move create through CreateExclusive; Update writes.
type failingSandbox struct {
	Sandbox
	failCreateAt int
	creates      int
	failWriteAt  int
	writes       int
	// partialWriteAt makes the Nth WriteFile truncate the file and write only
	// half the content before returning an error.
	partialWriteAt int
}

func (f *failingSandbox) CreateExclusive(ctx context.Context, path string, content []byte) error {
	f.creates++
	if f.creates == f.failCreateAt {
		return fmt.Errorf("simulated create failure")
	}
	return f.Sandbox.CreateExclusive(ctx, path, content)
}

func (f *failingSandbox) WriteFile(ctx context.Context, path string, content []byte) error {
	f.writes++
	if f.writes == f.failWriteAt {
		return fmt.Errorf("simulated write failure")
	}
	// Reproduce what a full disk or a dropped SFTP connection actually does: the
	// file is truncated and half the content lands, THEN the error comes back.
	// os.WriteFile is O_TRUNC, so a failed write is not a no-op.
	if f.writes == f.partialWriteAt {
		if err := f.Sandbox.WriteFile(ctx, path, content[:len(content)/2]); err != nil {
			return err
		}
		return fmt.Errorf("simulated partial write failure")
	}
	return f.Sandbox.WriteFile(ctx, path, content)
}

// Commit-phase rollback: when the 2nd write fails, the 1st (already committed)
// Add is undone from the in-memory snapshot.
func TestApplyPatchCommitRollback(t *testing.T) {
	ctx := context.Background()
	local := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	sb := &failingSandbox{Sandbox: local, failCreateAt: 2}

	patch := "*** Begin Patch\n" +
		"*** Add File: a.txt\n" +
		"+aaa\n" +
		"*** Add File: b.txt\n" +
		"+bbb\n" +
		"*** End Patch\n"

	if _, err := applyPatch(ctx, sb, patch); err == nil {
		t.Fatal("expected commit failure on 2nd write")
	}
	// a.txt was written then rolled back (RemoveFile via the real local sandbox).
	if _, err := local.ReadFile(ctx, "a.txt"); err == nil {
		t.Fatal("a.txt must have been rolled back")
	}
	if _, err := local.ReadFile(ctx, "b.txt"); err == nil {
		t.Fatal("b.txt was never written")
	}
}

// TestApplyPatchRestoresPartiallyWrittenFile locks the rollback of the op that
// fails ITSELF. WriteFile truncates before it writes, so a failure mid-write
// leaves the file damaged — but the failing op never enters `done`, so before
// this fix its undo never ran and a single-file Update reported "rolled back"
// over a truncated file.
func TestApplyPatchRestoresPartiallyWrittenFile(t *testing.T) {
	ctx := context.Background()
	local := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	const orig = "one\ntwo\nthree\n"
	if err := local.WriteFile(ctx, "a.txt", []byte(orig)); err != nil {
		t.Fatal(err)
	}
	// The setup write goes straight to `local`, so the wrapper's 1st write is the
	// patch's own update — the one we make fail halfway through.
	sb := &failingSandbox{Sandbox: local, partialWriteAt: 1}

	patch := "*** Begin Patch\n" +
		"*** Update File: a.txt\n" +
		" one\n" +
		"-two\n" +
		"+TWO\n" +
		" three\n" +
		"*** End Patch\n"

	if _, err := applyPatch(ctx, sb, patch); err == nil {
		t.Fatal("expected the partial write to fail the apply")
	}
	got, err := local.ReadFile(ctx, "a.txt")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != orig {
		t.Fatalf("file left damaged after a failed apply: %q, want %q", got, orig)
	}
}

// A Delete of a file too large to snapshot in memory parks it by renaming
// rather than failing: the commit removes the parked copy and a rollback puts
// the file back, byte for byte (spec §2.7s).
func TestApplyPatchDeleteLargeFileParks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: dir, MaxReadFileBytes: 16})
	big := bytes.Repeat([]byte("x"), 64)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	leftovers := func() []string {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".apply-patch.") {
				names = append(names, e.Name())
			}
		}
		return names
	}

	// Rollback: the Add that follows fails, so the parked file must come back.
	if err := os.WriteFile(filepath.Join(dir, "taken.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := applyPatch(ctx, sb, "*** Begin Patch\n*** Delete File: big.bin\n*** Add File: taken.txt\n+clobber\n*** End Patch\n")
	if err == nil {
		t.Fatal("adding over an existing file must fail the patch")
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "big.bin")); rerr != nil || !bytes.Equal(got, big) {
		t.Fatalf("big.bin after rollback = %d bytes, %v; want it restored intact", len(got), rerr)
	}
	if names := leftovers(); len(names) != 0 {
		t.Fatalf("rollback left parked copies behind: %v", names)
	}

	// Commit: the file is gone and so is its parked copy.
	out, err := applyPatch(ctx, sb, "*** Begin Patch\n*** Delete File: big.bin\n*** End Patch\n")
	if err != nil {
		t.Fatalf("deleting a large file: %v", err)
	}
	if !strings.Contains(out, "D big.bin") {
		t.Errorf("summary = %q, want it to report the delete", out)
	}
	if _, serr := os.Stat(filepath.Join(dir, "big.bin")); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("big.bin after the delete: %v", serr)
	}
	if names := leftovers(); len(names) != 0 {
		t.Fatalf("commit left parked copies behind: %v", names)
	}
}
