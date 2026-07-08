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

// TestRunHubResumeSameID locks the same-id resume contract: an interrupted
// run reopens under its own id, keeping the sequence counter, replay buffer,
// and attached subscribers — one logical run, one event stream.
func TestRunHubResumeSameID(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "ac", "sb"); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.publish("run1", env("run.started"))
	h.publish("run1", env("run.tool_call"))
	h.publish("run1", env("run.interrupted"))

	// A client attached during the first segment...
	var got []string
	if _, ok := h.Subscribe("run1", 3, func(e *protocol.Envelope) { got = append(got, e.Type) }); !ok {
		t.Fatal("subscribe failed")
	}

	h.finish("run1", true)
	if info, _ := h.Info("run1"); info.Status != RunInterrupted {
		t.Fatalf("status after pause = %q, want interrupted", info.Status)
	}
	if _, busy := h.ActiveRunForSession("sess1"); busy {
		t.Fatal("interrupted run must free the session slot")
	}

	// Resume reopens the same record: same id, seq continues, sub still fed.
	ctx, err := h.resume("run1", "sess1", "ac", "sb")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if ctx == nil {
		t.Fatal("resume returned nil context")
	}
	if info, _ := h.Info("run1"); info.Status != RunRunning {
		t.Fatalf("status after resume = %q, want running", info.Status)
	}
	if id, ok := h.ActiveRunForSession("sess1"); !ok || id != "run1" {
		t.Fatalf("session slot = %q,%v — want run1 reclaimed", id, ok)
	}
	h.publish("run1", env("run.started"))
	h.publish("run1", env("run.output"))
	if len(got) != 2 || got[0] != "run.started" || got[1] != "run.output" {
		t.Fatalf("existing subscriber missed resumed events: %v", got)
	}
	if info, _ := h.Info("run1"); info.LastSeq != 5 {
		t.Fatalf("seq restarted (LastSeq=%d), must continue from the first segment", info.LastSeq)
	}
	h.finish("run1", false)
	if info, _ := h.Info("run1"); info.Status != RunCompleted {
		t.Fatalf("final status = %q", info.Status)
	}
}

// A resume must respect the one-live-run-per-session invariant.
func TestRunHubResumeBusy(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("paused", "sess1", "", ""); err != nil {
		t.Fatalf("register paused: %v", err)
	}
	h.finish("paused", true)
	if _, _, err := h.register("blocker", "sess1", "", ""); err != nil {
		t.Fatalf("register blocker: %v", err)
	}
	if _, err := h.resume("paused", "sess1", "", ""); !errors.As(err, &ErrSessionBusy{}) {
		t.Fatalf("resume while busy: err = %v, want ErrSessionBusy", err)
	}
	h.finish("blocker", false)
	if _, err := h.resume("paused", "sess1", "", ""); err != nil {
		t.Fatalf("resume after free: %v", err)
	}
}

// The SSE close decision must key off IsFinalRunEvent, not IsTerminalRunEvent:
// a same-id resume leaves a historical run.interrupted in the buffer, and a
// late subscriber replaying from 0 must flow past it to the real run.output —
// treating the old interrupt as final would cut the stream short.
func TestFinalVsTerminalRunEvent(t *testing.T) {
	// run.interrupted is "terminal" (ends a segment) but NOT "final" (the run
	// resumes under the same id).
	if !IsTerminalRunEvent("run.interrupted") {
		t.Error("run.interrupted should be terminal (segment end)")
	}
	if IsFinalRunEvent("run.interrupted") {
		t.Error("run.interrupted must NOT be final — same-id resume continues the stream")
	}
	for _, typ := range []string{"run.output", "run.error", "run.cancelled"} {
		if !IsFinalRunEvent(typ) {
			t.Errorf("%s should be final", typ)
		}
	}
	if IsFinalRunEvent("run.step") {
		t.Error("run.step is not final")
	}

	// A resumed+completed run's buffer: interrupted (seq 2) then output (seq 4).
	// Replaying from 0, only the output is a final event — the SSE would close
	// on it, delivering everything including the resume's output.
	h := NewRunHub(context.Background())
	if _, _, err := h.register("r", "s", "", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.publish("r", env("run.started"))
	h.publish("r", env("run.interrupted"))
	h.finish("r", true)
	_, _ = h.resume("r", "s", "", "")
	h.publish("r", env("run.started"))
	h.publish("r", env("run.output"))

	var finals []int
	h.SubscribeSeq("r", 0, func(item SeqEnvelope) {
		if IsFinalRunEvent(item.Env.Type) {
			finals = append(finals, item.Seq)
		}
	})
	if len(finals) != 1 || finals[0] != 4 {
		t.Fatalf("final events on replay = %v, want [4] (the run.output, not the old interrupt)", finals)
	}
}

// After a restart (or retention GC) the hub has no record: resume registers a
// fresh one under the same id so the continuation still streams.
func TestRunHubResumeAfterRestart(t *testing.T) {
	h := NewRunHub(context.Background())
	ctx, err := h.resume("ghost-run", "sess1", "ac", "")
	if err != nil {
		t.Fatalf("resume without record: %v", err)
	}
	if ctx == nil {
		t.Fatal("nil context")
	}
	info, ok := h.Info("ghost-run")
	if !ok || info.Status != RunRunning || info.SessionID != "sess1" {
		t.Fatalf("fresh record wrong: %+v ok=%v", info, ok)
	}
}

// StopAfterTurn invokes the run's graceful-stop hook once installed, and reports
// false for a run that has none yet (the caller then falls back to a hard cancel).
func TestRunHubStopAfterTurn(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	// No hook installed yet.
	if h.StopAfterTurn("run1") {
		t.Error("StopAfterTurn should report false before a hook is installed")
	}
	called := 0
	h.setStopHook("run1", func() { called++ })
	if !h.StopAfterTurn("run1") {
		t.Error("StopAfterTurn should report true once a hook is installed")
	}
	if called != 1 {
		t.Errorf("stop hook called %d times, want 1", called)
	}
	// Unknown run.
	if h.StopAfterTurn("nope") {
		t.Error("StopAfterTurn on an unknown run should report false")
	}
}
