package bridge

import (
	"context"
	"sync"
	"testing"
)

// TestCancelResumeNoDataRace locks the fix: Cancel reads rec.cancel under
// rec.mu — the same lock resume takes to swap it to the new segment's cancel.
// Before the fix Cancel read the field lock-free, which both races resume's
// write (a data race) and can cancel the wrong segment (the old one after a
// resume already replaced it, or the freshly resumed one). Running Cancel and
// resume concurrently across many iterations lets -race catch an unsynchronized
// access.
func TestCancelResumeNoDataRace(t *testing.T) {
	for range 100 {
		h := NewRunHub(context.Background())
		seg, _, err := h.register("run1", "sess1", "", "", "", "", nil)
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
			if seg2, _, _, rerr := h.resume("run1", "sess1", "", "", "", "", nil); rerr == nil {
				seg2.finalize()
			}
		}()
		wg.Wait()
		seg.finalize()
	}
}
