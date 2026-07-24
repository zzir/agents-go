package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"
)

// slowCreateSandbox makes the Nth CreateExclusive block until ctx is done (to
// force a timeout mid-commit) and records RemoveFile calls only when their ctx
// is NOT already cancelled — so a rollback that inherited the cancelled commit
// ctx would record nothing.
type slowCreateSandbox struct {
	Sandbox
	mu       sync.Mutex
	removed  []string
	blockNth int
	n        int
}

func (s *slowCreateSandbox) CreateExclusive(ctx context.Context, _ string, _ []byte) error {
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	if n == s.blockNth {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (s *slowCreateSandbox) RemoveFile(ctx context.Context, p string) error {
	if err := ctx.Err(); err != nil {
		return err // a rollback must NOT run on the cancelled commit ctx
	}
	s.mu.Lock()
	s.removed = append(s.removed, p)
	s.mu.Unlock()
	return nil
}

// TestApplyPatchRollbackUsesDetachedContext locks the fix: when a commit fails
// because the original ctx timed out, the rollback runs on a fresh detached
// context so undo operations actually execute — rather than silently failing on
// the cancelled ctx and leaving a half-applied patch reported as rolled back.
func TestApplyPatchRollbackUsesDetachedContext(t *testing.T) {
	base := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	sb := &slowCreateSandbox{Sandbox: base, blockNth: 2}
	patch := "*** Begin Patch\n*** Add File: a.txt\n+aaa\n*** Add File: b.txt\n+bbb\n*** End Patch\n"

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := applyPatch(ctx, sb, patch); err == nil {
		t.Fatal("expected apply to fail on the timed-out second create")
	}

	sb.mu.Lock()
	removed := append([]string(nil), sb.removed...)
	sb.mu.Unlock()
	if len(removed) != 1 || removed[0] != "a.txt" {
		t.Fatalf("rollback should have removed a.txt on a detached ctx, got %v", removed)
	}
}
