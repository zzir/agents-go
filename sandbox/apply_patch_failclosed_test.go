package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// listErrSandbox wraps a Sandbox but forces ListDir to return a chosen error,
// to exercise targetExists's error handling.
type listErrSandbox struct {
	Sandbox
	err error
}

func (s listErrSandbox) ListDir(context.Context, string) ([]DirEntry, error) {
	return nil, s.err
}

// TestApplyPatchAddFailsClosedOnListError locks the fail-closed fix: when the
// existence check can't read the parent directory for a reason OTHER than "it
// doesn't exist" (permission denied, a transient backend error), Add must abort
// rather than treat the unreadable parent as "absent" and clobber a file that
// may well be there. Previously targetExists returned false on any error, so the
// add proceeded and overwrote.
func TestApplyPatchAddFailsClosedOnListError(t *testing.T) {
	ctx := context.Background()
	base := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	sb := listErrSandbox{Sandbox: base, err: errors.New("permission denied")}

	patch := "*** Begin Patch\n*** Add File: x.txt\n+hi\n*** End Patch\n"
	_, err := applyPatch(ctx, sb, patch)
	if err == nil {
		t.Fatal("expected a fail-closed error when ListDir fails for a non-NotExist reason")
	}
	// It should surface the underlying error, not the "already exists" guard.
	if strings.Contains(err.Error(), "already exists") {
		t.Errorf("wrong failure mode (should be the propagated list error): %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should propagate the ListDir failure, got: %v", err)
	}
}
