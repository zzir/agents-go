package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func newTaskRunner(t *testing.T) (*Runner, *store.TaskStore) {
	t.Helper()
	db := newTestDB(t)
	tasks := store.NewTaskStore(db)
	runner := NewRunner(context.Background(), db, &AgentDeps{
		AgentConfigs:     store.NewAgentConfigStore(db),
		Sessions:         store.NewSessionStore(db),
		Settings:         store.NewSettingStore(db),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: store.NewPendingApprovalStore(db),
		Tasks:            tasks,
	})
	return runner, tasks
}

func seedTask(t *testing.T, tasks *store.TaskStore, id, parent string) {
	t.Helper()
	if err := tasks.Create(context.Background(), &store.Task{
		ID: id, RunID: store.NewID(), ParentSessionID: parent, ChildSessionID: store.NewID(),
		Label: "t", Status: protocol.TaskWorking,
	}); err != nil {
		t.Fatal(err)
	}
}

// One bounded wait returns as soon as the task turns final — the polling
// loop's replacement.
func TestTaskStatusWaitReturnsOnCompletion(t *testing.T) {
	runner, tasks := newTaskRunner(t)
	seedTask(t, tasks, "task-1", "parent-1")

	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = tasks.Finalize(context.Background(), "task-1", protocol.TaskCompleted, "done", "full result")
	}()

	start := time.Now()
	info, err := runner.TaskStatus(context.Background(), "task-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != protocol.TaskCompleted || info.Result != "full result" {
		t.Fatalf("info = %+v, want completed with full result", info)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wait did not return promptly: %v", elapsed)
	}
}

// The wait window closing hands back the live (working) status.
func TestTaskStatusWaitTimesOutToWorking(t *testing.T) {
	runner, tasks := newTaskRunner(t)
	seedTask(t, tasks, "task-2", "parent-2")

	info, err := runner.TaskStatus(context.Background(), "task-2", 1)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != protocol.TaskWorking {
		t.Fatalf("status = %s, want working after timeout", info.Status)
	}
}

// A result delivered in-turn via task_status consumes the wake-up debt on the
// row: postRun's Finalize owed it (notify_state=pending), the read settles it.
func TestTaskStatusFinalConsumesNotification(t *testing.T) {
	runner, tasks := newTaskRunner(t)
	seedTask(t, tasks, "task-3", "parent-3")
	if won, err := tasks.Finalize(context.Background(), "task-3", protocol.TaskCompleted, "done", "r"); err != nil || !won {
		t.Fatalf("Finalize won=%v err=%v", won, err)
	}
	row, err := tasks.Get(context.Background(), "task-3")
	if err != nil {
		t.Fatal(err)
	}
	if row.NotifyState != store.NotifyPending {
		t.Fatalf("notify_state = %q, want pending after Finalize", row.NotifyState)
	}

	finishedAt := row.UpdatedAt

	if _, err := runner.TaskStatus(context.Background(), "task-3", 0); err != nil {
		t.Fatal(err)
	}
	row, err = tasks.Get(context.Background(), "task-3")
	if err != nil {
		t.Fatal(err)
	}
	if row.NotifyState != store.NotifyConsumed {
		t.Fatalf("notify_state = %q, want consumed after a final task_status read", row.NotifyState)
	}
	// Notification bookkeeping must not move the finish time: for a terminal
	// task updated_at is its end timestamp (the UI's duration comes from it).
	if !row.UpdatedAt.Equal(finishedAt) {
		t.Fatalf("updated_at moved by ConsumeNotify: %v -> %v", finishedAt, row.UpdatedAt)
	}
}

// The durable row is the terminal authority: a hub record that already shows
// completed while the row is still working (the Finalize hasn't landed) must
// NOT surface as final — that window used to return an empty result and eat
// the wake-up.
func TestTaskStatusHubTerminalWaitsForRow(t *testing.T) {
	runner, tasks := newTaskRunner(t)
	seedTask(t, tasks, "task-4", "parent-4")
	task, err := tasks.Get(context.Background(), "task-4")
	if err != nil {
		t.Fatal(err)
	}

	// Register the task's run and drive the hub record to completed without
	// touching the row — exactly the publish-before-persist window.
	meta := &TaskMeta{TaskID: task.ID, ParentSessionID: "parent-4"}
	if _, _, err := runner.hub.register(task.RunID, task.ChildSessionID, "", "", meta); err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.EventRunOutput, protocol.RunOutput{RunID: task.RunID, FinalOutput: "x"})
	if err != nil {
		t.Fatal(err)
	}
	runner.hub.publish(task.RunID, env)
	runner.hub.finish(task.RunID, false)

	info, err := runner.TaskStatus(context.Background(), "task-4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != protocol.TaskWorking {
		t.Fatalf("status = %s, want working while the row has not landed", info.Status)
	}
	row, err := tasks.Get(context.Background(), "task-4")
	if err != nil {
		t.Fatal(err)
	}
	if row.NotifyState != "" {
		t.Fatalf("notify_state = %q, want untouched (nothing consumed early)", row.NotifyState)
	}
}

// Stop and approve race on an input_required task through row CAS: the stop's
// Finalize and the approve's ReclaimWorking are mutually exclusive claims.
func TestStopApproveRowClaims(t *testing.T) {
	ctx := context.Background()
	_, tasks := newTaskRunner(t)

	// Stop wins first: the reclaim (approve) must lose.
	seedTask(t, tasks, "task-5", "parent-5")
	if err := tasks.MarkInputRequired(ctx, "task-5"); err != nil {
		t.Fatal(err)
	}
	won, err := tasks.Finalize(ctx, "task-5", protocol.TaskCancelled, "stopped", "")
	if err != nil || !won {
		t.Fatalf("stop Finalize won=%v err=%v", won, err)
	}
	won, err = tasks.ReclaimWorking(ctx, "task-5")
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("ReclaimWorking revived a cancelled task")
	}

	// Approve wins first: the resume proceeds (working), and a later terminal
	// transition (the run being cancelled) still lands exactly once.
	seedTask(t, tasks, "task-6", "parent-6")
	if err := tasks.MarkInputRequired(ctx, "task-6"); err != nil {
		t.Fatal(err)
	}
	won, err = tasks.ReclaimWorking(ctx, "task-6")
	if err != nil || !won {
		t.Fatalf("ReclaimWorking won=%v err=%v", won, err)
	}
	won, err = tasks.Finalize(ctx, "task-6", protocol.TaskCancelled, "stopped", "")
	if err != nil || !won {
		t.Fatalf("post-reclaim Finalize won=%v err=%v", won, err)
	}
	won, err = tasks.Finalize(ctx, "task-6", protocol.TaskCompleted, "late", "")
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("terminal state was overwritten")
	}
}
