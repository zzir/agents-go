package bridge

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A subscription's done channel closes once the broadcaster does — shutdown
// here, retention GC the same way — after every event reached the sink, so a
// stream over a run that never publishes a final event (interrupted, or aged
// out) still returns (workbench invariant 43).
func TestSubscribeDoneClosesWithTheBroadcaster(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	h.publish("run1", env("run.started"))
	h.publish("run1", env("run.interrupted"))
	h.finish("run1", true)

	sink := newAsyncSink()
	_, done, ok := h.SubscribeSeq("run1", 0, sink.seq)
	if !ok {
		t.Fatal("subscribe failed")
	}
	select {
	case <-done:
		t.Fatal("done closed while the broadcaster was still open")
	case <-time.After(50 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.Shutdown(ctx)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("done did not close after the broadcaster ended")
	}
	if sink.count() != 2 {
		t.Fatalf("delivered %d events before done, want both", sink.count())
	}
}

// The control handle is read and written under the record lock: an inject
// racing an aborted resume (which drops the handle) must not be a data race.
func TestControlHandleLockDiscipline(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	h.publish("run1", env("run.interrupted"))
	h.finish("run1", true)

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			seg, _, reopened, err := h.resume("run1", "sess1", "", "", "", nil)
			if err != nil {
				continue
			}
			h.abortResume("run1", seg, reopened)
		}
	})
	wg.Go(func() {
		for range 200 {
			_, _ = h.Inject("run1", "steer", "x")
			h.StopAfterTurn("run1")
			h.setControl("run1", nil)
		}
	})
	wg.Wait()
}
