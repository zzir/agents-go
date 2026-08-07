package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The task's OWN agent has to survive the round trip through the row, because
// a retry launches from the snapshot read back off it rather than from a fresh
// resolve. Losing it here started the new attempt with no agent config at all —
// invisibly, since a spawn passes the freshly resolved snapshot straight to the
// launcher and never reads this one back.
func TestTaskAdapter_InheritKeepsTheTaskAgent(t *testing.T) {
	ctx := context.Background()
	adapter := store.NewTaskAdapter(store.NewTaskStore(newTestDB(t)))

	in := &tasks.Task{
		ID: store.NewID(), RunID: store.NewID(),
		ParentSessionID: "parent", ChildSessionID: store.NewID(),
		Status: tasks.StatusWorking,
		Inherit: store.EncodeInherit(store.Inherit{
			AgentConfigID: "parent-agent",
			SandboxID:     "sandbox-1",
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
	if inherit.AgentConfigID != "parent-agent" || inherit.SandboxID != "sandbox-1" {
		t.Errorf("inherit = %+v, want the spawning run's setup too", inherit)
	}
}

// The claim is one conditional UPDATE, with the attempt ceiling in the WHERE
// clause so it holds across processes rather than only inside the caller that
// checked it.
func TestTaskStore_RetryClaim(t *testing.T) {
	ctx := context.Background()
	tasksStore := store.NewTaskStore(newTestDB(t))
	runID := seedTask(t, tasksStore, "task-r", "parent-r")

	// Working: nothing to resume.
	if won, err := tasksStore.RetryClaim(ctx, "task-r", store.NewID(), 3); err != nil || won {
		t.Fatalf("claimed a working task: won=%v err=%v", won, err)
	}
	if won, err := tasksStore.Finalize(ctx, "task-r", runID, protocol.TaskFailed, "boom", "boom"); err != nil || !won {
		t.Fatalf("failing the task: won=%v err=%v", won, err)
	}

	next := store.NewID()
	won, err := tasksStore.RetryClaim(ctx, "task-r", next, 3)
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	got, err := tasksStore.Get(ctx, "task-r")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != protocol.TaskWorking || got.RunID != next {
		t.Errorf("task = %s/%s, want working on the new run", got.Status, got.RunID)
	}
	if got.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", got.Attempt)
	}
	if got.Summary != "" || got.Result != "" || got.NotifyState != "" {
		t.Errorf("the failed attempt survived the claim: %+v", got)
	}

	// An unknown id is a different answer from a refused claim, and both
	// shipped stores must give the same one.
	if _, err := tasksStore.RetryClaim(ctx, "nope", store.NewID(), 3); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A stop that read the row before a retry names the attempt it saw and must
// lose: the run it would cancel is already over, and the live one would keep
// executing with its own result discarded.
func TestTaskStore_FinalizeIsBoundToTheAttempt(t *testing.T) {
	ctx := context.Background()
	tasksStore := store.NewTaskStore(newTestDB(t))
	stale := seedTask(t, tasksStore, "task-b", "parent-b")
	if won, err := tasksStore.Finalize(ctx, "task-b", stale, protocol.TaskFailed, "boom", ""); err != nil || !won {
		t.Fatalf("failing the task: won=%v err=%v", won, err)
	}
	if won, err := tasksStore.RetryClaim(ctx, "task-b", store.NewID(), 3); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}

	won, err := tasksStore.Finalize(ctx, "task-b", stale, protocol.TaskCancelled, "stopped", "")
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("a finalizer naming the previous attempt won")
	}
	got, err := tasksStore.Get(ctx, "task-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != protocol.TaskWorking {
		t.Errorf("status = %s, want the new attempt still working", got.Status)
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
	_, _, err := hub.register(store.NewID(), "child-x", "agent", "", meta)
	if !errors.As(err, new(ErrSessionDeleting)) {
		t.Fatalf("err = %v, want ErrSessionDeleting", err)
	}

	// A chat run on an unrelated session is unaffected.
	if _, _, err := hub.register(store.NewID(), "other", "agent", "", nil); err != nil {
		t.Fatalf("unrelated session refused: %v", err)
	}
}
