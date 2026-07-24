package sandbox

import (
	"context"
	"strings"
	"testing"
)

// adding a file that already exists must fail (codex apply_patch
// semantics) and must not overwrite the existing content. Without the guard the
// silent overwrite is bad enough on its own; worse, a later rollback would
// RemoveFile the clobbered original outright.
func TestApplyPatchAddExistingFails(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	seed(t, sb, "exists.txt", "ORIGINAL\n")

	patch := "*** Begin Patch\n" +
		"*** Add File: exists.txt\n" +
		"+OVERWRITE\n" +
		"*** End Patch\n"

	if _, err := applyPatch(ctx, sb, patch); err == nil {
		t.Fatal("expected an error adding over an existing file")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to mention 'already exists'", err)
	}
	if got := mustRead(t, sb, "exists.txt"); got != "ORIGINAL\n" {
		t.Errorf("existing file was clobbered: %q", got)
	}
}

// renaming (Move) onto an existing destination must fail, not silently
// overwrite it — otherwise the move's rollback would delete the pre-existing
// destination.
func TestApplyPatchMoveOntoExistingFails(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	seed(t, sb, "src.go", "package src\n")
	seed(t, sb, "dst.go", "package dst\n")

	patch := "*** Begin Patch\n" +
		"*** Update File: src.go\n" +
		"*** Move to: dst.go\n" +
		" package src\n" +
		"+// added\n" +
		"*** End Patch\n"

	if _, err := applyPatch(ctx, sb, patch); err == nil {
		t.Fatal("expected an error moving onto an existing file")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to mention 'already exists'", err)
	}
	// Both endpoints must be intact — the failure is caught in the plan phase,
	// before anything is written or deleted.
	if got := mustRead(t, sb, "src.go"); got != "package src\n" {
		t.Errorf("src.go changed: %q", got)
	}
	if got := mustRead(t, sb, "dst.go"); got != "package dst\n" {
		t.Errorf("dst.go clobbered: %q", got)
	}
}
