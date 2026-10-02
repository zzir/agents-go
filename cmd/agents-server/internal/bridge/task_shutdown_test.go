package bridge

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	sdktasks "github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// TestShutdownLeavesTaskForOrphanSweep locks the two cancellations apart: a
// shutdown ends a task's run with reason shutdown and leaves the row working,
// so the next start's orphan sweep fails it and owes the parent a turn. A
// person's stop stays cancelled with nothing owed (task_stop_test.go).
func TestShutdownLeavesTaskForOrphanSweep(t *testing.T) {
	ctx := context.Background()
	model := &endlessModel{arrived: make(chan struct{}, 1), gone: make(chan struct{})}
	srv := httptest.NewServer(model)
	defer srv.Close()

	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)
	fakeModelAgent(t, runner.db, agentConfigs, srv.URL)
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}

	info, err := runner.Tasks().Spawn(ctx, sdktasks.SpawnRequest{
		ParentSessionID: parent.ID, AgentName: "worker", Input: "run forever", Label: "probe",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-model.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the task run never reached the model")
	}
	time.Sleep(200 * time.Millisecond) // unambiguously mid-turn

	row, err := tasks.Get(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan *protocol.Envelope, 64)
	if _, ok := runner.hub.Subscribe(row.RunID, 0, func(env *protocol.Envelope) { events <- env }); !ok {
		t.Fatal("the task's run is not live in the hub")
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	runner.hub.Shutdown(shutdownCtx)

	if got := shutdownReason(t, events); got != protocol.RunCancelShutdown {
		t.Errorf("run.cancelled reason = %q, want %q", got, protocol.RunCancelShutdown)
	}
	if row, err = tasks.Get(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	if row.Status != "working" {
		t.Fatalf("row = %q after the shutdown, want working for the orphan sweep", row.Status)
	}
	if pending, err := runner.Deps.Wakeups.Pending(ctx, parent.ID); err != nil || len(pending) != 0 {
		t.Fatalf("parent owed %d wake-ups (err %v) after the shutdown, want none yet", len(pending), err)
	}

	// The next start: the sweep fails the row and owes the parent its turn.
	runner.FailOrphanedTasks(ctx)
	if row, err = tasks.Get(ctx, info.TaskID); err != nil {
		t.Fatal(err)
	}
	if row.Status != "failed" {
		t.Errorf("row = %q after the sweep, want failed", row.Status)
	}
	pending, err := runner.Deps.Wakeups.Pending(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("parent owed %d wake-ups after the sweep, want 1", len(pending))
	}
}

// shutdownReason returns the reason of the first run.cancelled among the
// events, failing when none arrives.
func shutdownReason(t *testing.T, events <-chan *protocol.Envelope) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-events:
			if env.Type != protocol.EventRunCancelled {
				continue
			}
			var p protocol.RunCancelled
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				t.Fatalf("run.cancelled payload %s: %v", env.Payload, err)
			}
			return p.Reason
		case <-deadline:
			t.Fatal("no run.cancelled reached the subscriber")
			return ""
		}
	}
}
