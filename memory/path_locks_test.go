package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// TestPathLocks_ConcurrentAddItemsSamePath opens one FileSession per goroutine
// on the same path and appends concurrently: the per-path lock must serialize
// the writes so none are lost.
func TestPathLocks_ConcurrentAddItemsSamePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			sess, err := NewFileSession(dir, "same-path")
			if err != nil {
				t.Error(err)
				return
			}
			if err := sess.AddItems(ctx, agents.InputItemsFromText(fmt.Sprintf("msg-%d", i))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	sess, err := NewFileSession(dir, "same-path")
	if err != nil {
		t.Fatal(err)
	}
	items, err := sess.GetItems(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != goroutines {
		t.Errorf("got %d items, want %d (lost writes)", len(items), goroutines)
	}
	if n := lockTableSize(); n != 0 {
		t.Errorf("lock table has %d entries after all operations finished, want 0", n)
	}
}

// TestPathLocks_TableShrinksToZero churns through many distinct session paths
// concurrently and verifies the reference-counted lock table retains no
// entries once every operation has released its lock.
func TestPathLocks_TableShrinksToZero(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	const sessions = 20
	var wg sync.WaitGroup
	wg.Add(sessions)
	for i := range sessions {
		go func() {
			defer wg.Done()
			sess, err := NewFileSession(dir, fmt.Sprintf("conv-%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			if err := sess.AddItems(ctx, agents.InputItemsFromText("hello")); err != nil {
				t.Error(err)
				return
			}
			if _, err := sess.GetItems(ctx, 0); err != nil {
				t.Error(err)
				return
			}
			if err := sess.Clear(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if n := lockTableSize(); n != 0 {
		t.Errorf("lock table has %d entries after all sessions finished, want 0", n)
	}
}
