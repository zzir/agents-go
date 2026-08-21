package bridge

import (
	"context"
	"testing"
	"time"
)

// A panicking run segment must fail that run alone: the goroutine recovers,
// the session slot frees, and the process survives to serve everything else.
func TestLaunchSegmentRecoversPanic(t *testing.T) {
	hub := NewRunHub(context.Background())
	seg, _, err := hub.register("run-1", "sess-1", "agent", "", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	r := &Runner{hub: hub}
	r.launchSegment(seg, "run-1", "sess-1", nil, func() *RunOutcome {
		panic("boom")
	})
	hub.waitDone("run-1", time.Now().Add(2*time.Second))
	if _, busy := hub.ActiveRunForSession("sess-1"); busy {
		t.Fatal("session slot still held after a panicking segment")
	}
	// The slot must be genuinely reusable, not just unlisted.
	seg2, _, err := hub.register("run-2", "sess-1", "agent", "", "", nil)
	if err != nil {
		t.Fatalf("register after panic: %v", err)
	}
	hub.unregister("run-2", seg2)
}
