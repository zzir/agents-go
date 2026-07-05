package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

func env(typ string) *protocol.Envelope { return &protocol.Envelope{Type: typ} }

func TestRunHubBufferAndReplay(t *testing.T) {
	h := NewRunHub(context.Background())
	rec, _, err := h.register("run1", "sess1", "", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if rec.info.Status != RunRunning {
		t.Fatalf("new run status = %q", rec.info.Status)
	}

	h.publish("run1", env("run.started"))
	h.publish("run1", env("run.step"))

	// A subscriber attaching mid-run replays the buffered events, then sees
	// live ones.
	var got []string
	subID, ok := h.Subscribe("run1", 0, func(e *protocol.Envelope) { got = append(got, e.Type) })
	if !ok {
		t.Fatal("subscribe failed")
	}
	if len(got) != 2 || got[0] != "run.started" || got[1] != "run.step" {
		t.Fatalf("replay wrong: %v", got)
	}
	h.publish("run1", env("run.output"))
	if len(got) != 3 || got[2] != "run.output" {
		t.Fatalf("live delivery wrong: %v", got)
	}

	// Terminal event advanced status; finish keeps it.
	h.finish("run1", false)
	if info, _ := h.Info("run1"); info.Status != RunCompleted {
		t.Fatalf("status after output = %q, want completed", info.Status)
	}

	// from_seq replays only events after the cursor.
	var after []int
	h.SubscribeSeq("run1", 2, func(item SeqEnvelope) { after = append(after, item.Seq) })
	if len(after) != 1 || after[0] != 3 {
		t.Fatalf("from_seq replay wrong: %v", after)
	}

	h.Unsubscribe("run1", subID)
	h.publish("run1", env("run.step"))
	if len(got) != 3 {
		t.Fatalf("unsubscribed sink still received events: %v", got)
	}
}

func TestRunHubSessionBusy(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", ""); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, _, err := h.register("run2", "sess1", "", "")
	if err == nil {
		t.Fatal("expected ErrSessionBusy for second live run on same session")
	}
	var busy ErrSessionBusy
	if !errors.As(err, &busy) || busy.RunID != "run1" {
		t.Fatalf("wrong error: %v", err)
	}

	// After the first run finishes, the session frees up.
	h.finish("run1", false)
	if _, _, err := h.register("run2", "sess1", "", ""); err != nil {
		t.Fatalf("register after finish: %v", err)
	}
}

func TestRunHubCancelAndInterrupt(t *testing.T) {
	h := NewRunHub(context.Background())
	_, ctx, err := h.register("run1", "sess1", "", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if !h.Cancel("run1") {
		t.Fatal("cancel should report the run existed")
	}
	if ctx.Err() == nil {
		t.Fatal("cancel should have cancelled the run context")
	}
	if h.Cancel("nope") {
		t.Fatal("cancel of unknown run should report false")
	}

	// Interrupt path sets the interrupted status.
	h.finish("run1", true)
	if info, _ := h.Info("run1"); info.Status != RunInterrupted {
		t.Fatalf("status after interrupt = %q, want interrupted", info.Status)
	}
}

func TestRunHubInterruptedEventIsTerminal(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", ""); err != nil {
		t.Fatal(err)
	}
	h.publish("run1", env("run.interrupted"))
	if info, _ := h.Info("run1"); info.Status != RunInterrupted {
		t.Fatalf("run.interrupted should set status, got %q", info.Status)
	}

	for _, typ := range []string{"run.output", "run.error", "run.cancelled", "run.interrupted"} {
		if !IsTerminalRunEvent(typ) {
			t.Errorf("%s should be terminal", typ)
		}
	}
	if IsTerminalRunEvent("run.step") {
		t.Error("run.step must not be terminal")
	}
}
