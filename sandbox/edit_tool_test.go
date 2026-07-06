package sandbox

import (
	"context"
	"fmt"
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

// failingSandbox fails the Nth WriteFile so commit-phase rollback can be tested.
type failingSandbox struct {
	Sandbox
	failWriteAt int
	writes      int
}

func (f *failingSandbox) WriteFile(ctx context.Context, path string, content []byte) error {
	f.writes++
	if f.writes == f.failWriteAt {
		return fmt.Errorf("simulated write failure")
	}
	return f.Sandbox.WriteFile(ctx, path, content)
}

// Commit-phase rollback: when the 2nd write fails, the 1st (already committed)
// Add is undone from the in-memory snapshot.
func TestApplyPatchCommitRollback(t *testing.T) {
	ctx := context.Background()
	local := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	sb := &failingSandbox{Sandbox: local, failWriteAt: 2}

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
