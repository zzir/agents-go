package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func seedTask(t *testing.T, tasks *store.TaskStore, id, parent string) {
	t.Helper()
	if err := tasks.Create(context.Background(), &store.Task{
		ID: id, RunID: store.NewID(), ParentSessionID: parent, ChildSessionID: store.NewID(),
		Label: "t", Status: protocol.TaskWorking,
	}); err != nil {
		t.Fatal(err)
	}
}

// Stop and approve race on an input_required task through row CAS: the stop's
// Finalize and the approve's ReclaimWorking are mutually exclusive claims.
func TestStopApproveRowClaims(t *testing.T) {
	ctx := context.Background()
	tasks := store.NewTaskStore(newTestDB(t))

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
