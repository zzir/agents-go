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
		ID: id, ParentSessionID: parent, ChildSessionID: store.NewID(),
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
		_ = tasks.SetStatus(context.Background(), "task-1", protocol.TaskCompleted, "done", "full result")
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

// A result delivered in-turn via task_status consumes the wake-up: postRun
// must not queue a duplicate notification for it.
func TestTaskStatusFinalConsumesNotification(t *testing.T) {
	runner, tasks := newTaskRunner(t)
	seedTask(t, tasks, "task-3", "parent-3")
	_ = tasks.SetStatus(context.Background(), "task-3", protocol.TaskCompleted, "done", "r")

	if _, err := runner.TaskStatus(context.Background(), "task-3", 0); err != nil {
		t.Fatal(err)
	}
	runner.notifMu.Lock()
	delivered := runner.deliveredResults["task-3"]
	runner.notifMu.Unlock()
	if !delivered {
		t.Fatal("final task_status did not mark the result as delivered")
	}

	// queued entry (had the completion raced ahead) is dropped too
	runner.queueTaskNotification("parent-3", "task-3")
	runner.consumeTaskNotification("parent-3", "task-3")
	runner.notifMu.Lock()
	queue := runner.pendingNotifs["parent-3"]
	runner.notifMu.Unlock()
	if len(queue) != 0 {
		t.Fatalf("queue = %v, want the consumed task dropped", queue)
	}
}
