package bridge

import (
	"context"
	"sync"
	"testing"
)

// TestCancelResumeNoDataRace: Cancel reads rec.cancel under rec.mu, the lock
// resume takes to swap it; Cancel and resume run concurrently so -race catches
// an unsynchronized access.
func TestCancelResumeNoDataRace(t *testing.T) {
	for range 100 {
		h := NewRunHub(context.Background())
		seg, _, err := h.register("run1", "sess1", "", "", "", nil)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		// Interrupted: the record is eligible for resume, so Cancel and resume
		// can genuinely contend over rec.cancel.
		h.finish("run1", true)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.Cancel("run1")
		}()
		go func() {
			defer wg.Done()
			if seg2, _, _, rerr := h.resume("run1", "sess1", "", "", "", nil); rerr == nil {
				seg2.finalize()
			}
		}()
		wg.Wait()
		seg.finalize()
	}
}
