package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A resume's build is the segment's to release, exactly once: when the
// segment's preamble panics (before its own recover is armed) and when the
// segment ends normally.
func TestExecStreamedReleasesAResumeBuildOnce(t *testing.T) {
	ctx := context.Background()

	t.Run("preamble panic", func(t *testing.T) {
		// No hub: the preamble's first hub read panics, ahead of the recover.
		runner := &Runner{Deps: &AgentDeps{}}
		released := 0
		built := &BuildResult{releaseSandbox: func() { released++ }}
		panicked := func() (p any) {
			defer func() { p = recover() }()
			runner.execStreamed(ctx, store.NewID(), store.NewID(), "agent", "", segmentSpec{built: built})
			return nil
		}()
		if panicked == nil {
			t.Fatal("the preamble did not panic; the test no longer reaches the case it is for")
		}
		if released != 1 {
			t.Fatalf("release called %d times over a panicking segment, want 1", released)
		}
	})

	t.Run("segment ends", func(t *testing.T) {
		runner, _, _, _ := newTaskTestRunner(t)
		runID, sessID := store.NewID(), store.NewID() // absent: the segment ends at its session read
		seg, sctx, err := runner.hub.register(runID, sessID, "", "agent", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer seg.finalize()
		released := 0
		built := &BuildResult{Agent: &agents.Agent{Name: "a", Model: "m"}, Provider: neverProvider{}, releaseSandbox: func() { released++ }}
		if out := runner.execStreamed(sctx, runID, sessID, "agent", "", segmentSpec{failCode: protocol.CodeResumeError, built: built}); out.ErrCode == "" {
			t.Fatalf("outcome = %+v, want a failure", out)
		}
		if released != 1 {
			t.Fatalf("release called %d times over one segment, want 1", released)
		}
	})
}

// A panic that escapes a segment still ends the run properly: run.error is
// published (the hub reads errored), postRun runs (the session's wake-up
// debts are drained), and onDone receives the failure.
func TestSegmentPanicStillRunsTeardown(t *testing.T) {
	ctx := context.Background()
	runner, sessions, _, _ := newTaskTestRunner(t)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// A debt with no agent config is cancelled by the drain: proof postRun ran.
	if err := runner.Deps.Wakeups.Owe(ctx, &store.Wakeup{SessionID: sess.ID, Kind: store.WakeKindTask, Payload: "p"}); err != nil {
		t.Fatal(err)
	}
	runID := store.NewID()
	seg, _, err := runner.hub.register(runID, sess.ID, "", "agent", "proj", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *RunOutcome, 1)
	runner.launchSegment(seg, runID, sess.ID, func(o *RunOutcome) { done <- o }, func() *RunOutcome { panic("preamble boom") })

	var out *RunOutcome
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone never fired after the panic")
	}
	if out.ErrCode != protocol.CodeInternal || out.RunID != runID || out.AgentConfigID != "agent" || out.ProjectID != "proj" {
		t.Fatalf("onDone got %+v, want an internal failure carrying the run's identity", out)
	}
	runner.hub.waitDone(runID, time.Now().Add(5*time.Second))
	if info, _ := runner.hub.Info(runID); info.Status != RunErrored {
		t.Fatalf("hub status = %q, want errored", info.Status)
	}
	if _, busy := runner.hub.ActiveRunForSession(sess.ID); busy {
		t.Fatal("session slot still held after the panic")
	}
	if pending, err := runner.Deps.Wakeups.Pending(ctx, sess.ID); err != nil || len(pending) != 0 {
		t.Fatalf("pending wake-ups after the panic = %v (err %v); postRun did not drain", pending, err)
	}
}
