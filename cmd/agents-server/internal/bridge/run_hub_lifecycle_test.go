package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// TestSegmentFinalizeNoDoubleClose locks the fix: each run segment owns its
// own done gate, so a resume that swaps a fresh segment onto the record while
// the old segment's goroutine is still winding down (the window between finish
// and finalize) cannot make two goroutines close one channel. The crux is that
// finalizing the OLD segment must NOT close the NEW segment's gate — the exact
// cross-close the previous markDone(runID) design produced, panicking when the
// new segment then closed its (already-closed) gate.
func TestSegmentFinalizeNoDoubleClose(t *testing.T) {
	h := NewRunHub(context.Background())
	seg1, _, err := h.register("run1", "sess1", "", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Segment 1 pauses for approval and frees the session slot, but its
	// goroutine has NOT finalized yet — postRun/onDone still running.
	h.publish("run1", env("run.interrupted"))
	h.finish("run1", true)

	// A racing approve resumes: a fresh segment (new done gate) is swapped onto
	// the record while seg1 is still in flight.
	seg2, _, err := h.resume("run1", "sess1", "", "", nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if seg1.done == seg2.done {
		t.Fatal("resume reused the old segment's done gate — segments must own distinct gates")
	}

	// Finalizing the OLD segment must close only its OWN gate.
	seg1.finalize()
	select {
	case <-seg2.done:
		t.Fatal("finalizing the old segment closed the new segment's gate (the double-close)")
	default:
	}
	select {
	case <-seg1.done:
	default:
		t.Fatal("seg1 gate not closed by its own finalize")
	}

	// The new segment finalizes independently; repeat finalize is idempotent.
	seg2.finalize()
	seg1.finalize()
	seg2.finalize()
	select {
	case <-seg2.done:
	default:
		t.Fatal("seg2 gate not closed by its own finalize")
	}
}

// TestSegmentFinalizeReleasesContext locks the fix: a segment's finalize
// cancels its own context, so a normally-finishing run no longer leaks its
// cancel context as a permanent child of the hub root.
func TestSegmentFinalizeReleasesContext(t *testing.T) {
	h := NewRunHub(context.Background())
	seg, ctx, err := h.register("run1", "sess1", "", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("fresh segment context should not be cancelled")
	}
	seg.finalize()
	if ctx.Err() == nil {
		t.Fatal("finalize must cancel the segment context (else it leaks as a hub-root child)")
	}
}

// TestResumeClearsStaleStopHook locks: a resume must drop the previous
// segment's graceful-stop hook. A stale hook closed over the OLD StreamedResult
// and would stop the wrong stream — so after a resume, StopAfterTurn must find
// no hook (returns false) until the new segment installs its own.
func TestResumeClearsStaleStopHook(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", nil); err != nil {
		t.Fatal(err)
	}
	stale := 0
	h.setStopHook("run1", func() { stale++ })
	h.finish("run1", true)

	if _, _, err := h.resume("run1", "sess1", "", "", nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// The stale hook must be gone: no hook installed on the fresh segment yet.
	if h.StopAfterTurn("run1") {
		t.Fatal("resume did not clear the stale stop hook ")
	}
	if stale != 0 {
		t.Fatalf("stale hook fired %d times after resume; must be dropped", stale)
	}
	// The new segment installs its own hook, which then works.
	fresh := 0
	h.setStopHook("run1", func() { fresh++ })
	if !h.StopAfterTurn("run1") || fresh != 1 {
		t.Fatalf("fresh stop hook not honored: called=%d", fresh)
	}
}

// TestRegisterRefusesDeletingSession locks: once a session's delete cascade
// begins, no new run — fresh or resumed — may be registered on it, so a task's
// postRun drain (or a late resume) can't start a run that outlives the delete.
func TestRegisterRefusesDeletingSession(t *testing.T) {
	h := NewRunHub(context.Background())
	h.markSessionDeleting("sess1")
	if !h.SessionDeleting("sess1") {
		t.Fatal("SessionDeleting should report true after markSessionDeleting")
	}

	_, _, err := h.register("run1", "sess1", "", "", nil)
	if !errors.As(err, &ErrSessionDeleting{}) {
		t.Fatalf("register on a deleting session: err = %v, want ErrSessionDeleting", err)
	}
	if _, _, err := h.resume("run1", "sess1", "", "", nil); !errors.As(err, &ErrSessionDeleting{}) {
		t.Fatalf("resume on a deleting session: err = %v, want ErrSessionDeleting", err)
	}
	// A different session is unaffected.
	if _, _, err := h.register("run2", "other", "", "", nil); err != nil {
		t.Fatalf("register on a live session: %v", err)
	}
}

// TestPublishSeqOrderingUnderConcurrency locks: with the whole publish
// serialized (seq assignment through fan-out), a single subscriber can never
// observe a higher seq before a lower one, even when many goroutines publish at
// once. Run under -race, a lock-free fan-out would reorder deliveries.
func TestPublishSeqOrderingUnderConcurrency(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", nil); err != nil {
		t.Fatal(err)
	}

	sink := newAsyncSink()
	if _, ok := h.SubscribeSeq("run1", 0, sink.seq); !ok {
		t.Fatal("subscribe failed")
	}

	const n = 200
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.publish("run1", env(protocol.EventRunStep))
		}()
	}
	wg.Wait()

	if !sink.wait(t, n) {
		t.Fatalf("subscriber received %d events, want %d", sink.count(), n)
	}
	seqs := sink.gotSeqs()
	// A single subscriber's deliveries must be strictly increasing in seq:
	// Fanout holds its publish lock across sequence assignment AND delivery, so
	// the two are one atomic step.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("out-of-order delivery at %d: seq %d after %d", i, seqs[i], seqs[i-1])
		}
	}
}

// TestWaitDoneTracksLiveSegment locks the settle contract ResolveApproval leans
// on: waitDone blocks on the CURRENT segment and returns once it finalizes.
func TestWaitDoneTracksLiveSegment(t *testing.T) {
	h := NewRunHub(context.Background())
	seg, _, err := h.register("run1", "sess1", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		h.waitDone("run1", time.Now().Add(2*time.Second))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("waitDone returned before the segment finalized")
	case <-time.After(50 * time.Millisecond):
	}
	seg.finalize()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitDone did not return after the segment finalized")
	}
}
