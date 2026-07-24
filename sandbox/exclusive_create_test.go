package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"
)

// TestCreateExclusiveConcurrentOnlyOneWins locks the atomic exclusive-create
// that closes apply_patch's TOCTOU: with many goroutines creating the same new
// path, exactly one succeeds and the rest get fs.ErrExist. Before this, two
// concurrent Add/Move of the same target could both observe "absent", both
// write, and one's rollback could RemoveFile the other's file.
func TestCreateExclusiveConcurrentOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, exist, other := 0, 0, 0
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := sb.CreateExclusive(ctx, "race.txt", []byte{byte('a' + i)})
			mu.Lock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, fs.ErrExist):
				exist++
			default:
				other++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if ok != 1 {
		t.Fatalf("exactly one create must win, got %d", ok)
	}
	if exist != n-1 {
		t.Fatalf("the other %d creates must fail with fs.ErrExist, got %d (and %d other errors)", n-1, exist, other)
	}
}

// TestCreateExclusiveRejectsExistingAndKeepsContent confirms a second create of
// an existing path fails with fs.ErrExist and does not overwrite.
func TestCreateExclusiveRejectsExistingAndKeepsContent(t *testing.T) {
	ctx := context.Background()
	sb := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})

	if err := sb.CreateExclusive(ctx, "x.txt", []byte("first")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := sb.CreateExclusive(ctx, "x.txt", []byte("second")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: want fs.ErrExist, got %v", err)
	}
	got, err := sb.ReadFile(ctx, "x.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("content clobbered: %q", got)
	}
}

// TestApplyPatchLockPerSandbox verifies the apply_patch lock is keyed on the
// Sandbox instance: the same sandbox shares one lock (so applies serialize even
// across separately-built tools/runs sharing that Sandbox), and different
// sandboxes get different locks.
func TestApplyPatchLockPerSandbox(t *testing.T) {
	sb1 := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	sb2 := NewLocalWithOptions(LocalOptions{WorkDir: t.TempDir()})
	l1a := applyPatchLock(sb1)
	l1b := applyPatchLock(sb1)
	if l1a != l1b {
		t.Fatal("the same sandbox must share one apply_patch lock")
	}
	if l1a == applyPatchLock(sb2) {
		t.Fatal("different sandboxes must not share an apply_patch lock")
	}
}
