package bridge

import (
	"context"
	"errors"
	"testing"
)

// A fence (reserveSessions) refuses while any of its sessions has a live run,
// and while it holds, register and resume refuse those sessions — a task run
// whose parent is fenced included; release lets them through again.
func TestReserveSessionsFencesRegisterAndResume(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	_, err := h.reserveSessions("sess1", "sess2")
	var busy ErrSessionBusy
	if !errors.As(err, &busy) || busy.RunID != "run1" {
		t.Fatalf("reserve over a live run: err = %v, want ErrSessionBusy{run1}", err)
	}
	h.finish("run1", true) // paused: resumable once the fence lifts

	release, err := h.reserveSessions("sess1", "sess2")
	if err != nil {
		t.Fatalf("reserve at rest: %v", err)
	}
	if _, _, err := h.register("run2", "sess1", "", "", "", nil); !errors.As(err, &busy) {
		t.Fatalf("register on a fenced session: err = %v, want ErrSessionBusy", err)
	}
	if _, _, _, err := h.resume("run1", "sess1", "", "", "", nil); !errors.As(err, &busy) {
		t.Fatalf("resume on a fenced session: err = %v, want ErrSessionBusy", err)
	}
	if _, _, err := h.register("run3", "child", "", "", "", &TaskMeta{ParentSessionID: "sess1"}); !errors.As(err, &busy) {
		t.Fatalf("task run under a fenced parent: err = %v, want ErrSessionBusy", err)
	}
	if _, err := h.reserveSessions("sess2", "other"); !errors.As(err, &busy) {
		t.Fatalf("a second fence over a fenced session: err = %v, want ErrSessionBusy", err)
	}
	if seg, _, err := h.register("run4", "other", "", "", "", nil); err != nil {
		t.Fatalf("register on an unfenced session: %v", err)
	} else {
		h.unregister("run4", seg)
	}

	release()
	release() // idempotent
	if seg, _, err := h.register("run2", "sess2", "", "", "", nil); err != nil {
		t.Fatalf("register after release: %v", err)
	} else {
		h.unregister("run2", seg)
	}
	if _, _, _, err := h.resume("run1", "sess1", "", "", "", nil); err != nil {
		t.Fatalf("resume after release: %v", err)
	}
}
