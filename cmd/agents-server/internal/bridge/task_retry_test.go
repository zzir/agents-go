package bridge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The task's OWN agent has to survive the round trip through the row, because
// a retry launches from the snapshot read back off it rather than from a fresh
// resolve. Losing it here started the new attempt with no agent config at all —
// invisibly, since a spawn passes the freshly resolved snapshot straight to the
// launcher and never reads this one back.
func TestTaskAdapter_InheritKeepsTheTaskAgent(t *testing.T) {
	ctx := context.Background()
	adapter := store.NewTaskAdapter(store.NewTaskStore(testdb.New(t)))

	in := &tasks.Task{
		ID: store.NewID(), RunID: store.NewID(),
		ParentSessionID: "parent", ChildSessionID: store.NewID(),
		Status: tasks.StatusWorking,
		Inherit: store.EncodeInherit(store.Inherit{
			AgentConfigID: "parent-agent",
			SandboxID:     "sandbox-1",
			ProjectID:     "proj-1",
			TaskAgentID:   "task-agent",
		}),
	}
	if err := adapter.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := adapter.Get(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	inherit := store.DecodeInherit(got.Inherit)
	if inherit.TaskAgentID != "task-agent" {
		t.Errorf("task agent = %q, want it preserved — a retry launches from this snapshot", inherit.TaskAgentID)
	}
	if inherit.AgentConfigID != "parent-agent" || inherit.SandboxID != "sandbox-1" || inherit.ProjectID != "proj-1" {
		t.Errorf("inherit = %+v, want the spawning run's setup too", inherit)
	}
}

// The Manager needs more than "no error" from a stop: a host that has never
// heard of the run has nothing to report but success, and reading that as "it
// will wind itself up" leaves a task running that someone was told was
// stopped. A run that is already over is not a cancellation either — saying so
// would rewrite an outcome every client has seen.
func TestTaskStopper_ReportsWhatItActuallyDid(t *testing.T) {
	ctx := context.Background()
	runner, _, _, _ := newTaskTestRunner(t)
	stopper := taskStopper{runner}

	out, err := stopper.Stop(ctx, "never-started", false)
	if err != nil {
		t.Fatal(err)
	}
	if out != tasks.StopUnknownRun {
		t.Errorf("outcome = %v, want StopUnknownRun for a run the hub has no record of", out)
	}

	// A live run takes a real cancel, and a graceful stop says it will report
	// its own ending.
	seg, _, err := runner.hub.register("live-run", "sess-live", "", "agent", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seg.finalize()
	runner.hub.setControl("live-run", &fakeControl{})
	if out, err = stopper.Stop(ctx, "live-run", true); err != nil || out != tasks.StopAfterTurn {
		t.Errorf("graceful stop of a live run = %v (err %v), want StopAfterTurn", out, err)
	}
	// Without a control there is nothing to defer to, so the run is cancelled
	// outright — and saying AfterTurn would promise an ending nobody records.
	seg3, _, err := runner.hub.register("no-ctrl", "sess-noctrl", "", "agent", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seg3.finalize()
	if out, err = stopper.Stop(ctx, "no-ctrl", true); err != nil || out != tasks.StopCancelled {
		t.Errorf("graceful stop with no control = %v (err %v), want StopCancelled", out, err)
	}

	// A record the hub keeps after the run ended: nothing to cancel, and
	// nothing to announce.
	seg2, _, err := runner.hub.register("done-run", "sess-done", "", "agent", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	seg2.finalize()
	runner.hub.finish("done-run", false)
	if info, ok := runner.hub.Info("done-run"); !ok || info.Status != RunCompleted {
		t.Fatalf("precondition: the run is %+v, want a retained completed record", info)
	}
	// Atomic: the sink runs on the subscription's goroutine, unjoined.
	var published atomic.Int32
	detach, ok := runner.hub.Subscribe("done-run", 0, func(*protocol.Envelope) { published.Add(1) })
	if !ok {
		t.Fatal("cannot watch the finished run")
	}
	defer detach()

	if out, err = stopper.Stop(ctx, "done-run", false); err != nil || out == tasks.StopAfterTurn {
		t.Errorf("stop of a finished run = %v (err %v), want it reported as over", out, err)
	}
	if info, _ := runner.hub.Info("done-run"); info.Status != RunCompleted {
		t.Errorf("the finished run became %q — a stop rewrote an outcome clients already saw", info.Status)
	}
	if n := published.Load(); n != 0 {
		t.Errorf("a finished run published %d event(s) on being stopped", n)
	}
}

// A failed task is invisible to a session teardown — its run is over and its
// hidden session was never marked — so the hub has to refuse a run whose PARENT
// is being deleted. Otherwise a retry landing between the mark and the cascade
// starts a run that writes into rows the cascade is removing.
func TestRunHub_RefusesATaskRunWhoseParentIsBeingDeleted(t *testing.T) {
	runner, _, _, _ := newTaskTestRunner(t)
	hub := runner.hub
	hub.markSessionDeleting("parent-x")

	meta := &TaskMeta{TaskID: "task-x", ParentSessionID: "parent-x", Attempt: 2}
	_, _, err := hub.register(store.NewID(), "child-x", "", "agent", "", "", meta)
	if !errors.As(err, new(ErrSessionDeleting)) {
		t.Fatalf("err = %v, want ErrSessionDeleting", err)
	}

	// A chat run on an unrelated session is unaffected.
	if _, _, err := hub.register(store.NewID(), "other", "", "agent", "", "", nil); err != nil {
		t.Fatalf("unrelated session refused: %v", err)
	}
}
