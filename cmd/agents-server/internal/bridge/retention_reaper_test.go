package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// An expired step approval ends its execution — cancelled against the
// approval's own attempt — even when the child session cannot take the
// banner, and the change is announced to the clients.
func TestApprovalReaperEndsThePausedTaskAndAnnouncesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner, sessions, tasks, _ := newTaskTestRunner(t)
	if err := store.NewSettingStore(runner.db).Set(ctx, settings.KeyApprovalTTLMinutes, "1"); err != nil {
		t.Fatal(err)
	}
	parent := &store.Session{ID: store.NewID(), Name: "p"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	// The child session is deliberately NOT created: the banner has nowhere
	// to go, which must not stop the task from being ended.
	row := &store.Task{
		ID: store.NewID(), RunID: store.NewID(), Kind: store.TaskKindWorkflow, State: []byte(`{"steps":[],"step_id":"x"}`),
		ParentSessionID: parent.ID, ChildSessionID: store.NewID(), Label: "wf", Status: "input_required",
	}
	if err := tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	pending := &store.PendingApproval{RunID: row.RunID, SessionID: row.ChildSessionID, Kind: store.ApprovalKindStep, AgentConfigID: "a"}
	if err := runner.Deps.PendingApprovals.Save(ctx, pending); err != nil {
		t.Fatal(err)
	}
	// Age the approval past the TTL.
	if _, err := runner.db.NewUpdate().Model((*store.PendingApproval)(nil)).
		Set("created_at = ?", time.Now().Add(-2*time.Hour)).Where("run_id = ?", row.RunID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var announced []string
	go RunApprovalReaper(ctx, runner.Deps.Settings, runner.Deps.PendingApprovals, store.NewSharedEntryStore(runner.db), tasks,
		func(_ context.Context, id string) { mu.Lock(); announced = append(announced, id); mu.Unlock() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		after, err := tasks.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task = %s, want cancelled by the reaper", after.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := runner.Deps.PendingApprovals.Get(ctx, row.RunID); err == nil {
		t.Fatal("the expired approval must be gone")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(announced) != 1 || announced[0] != row.ID {
		t.Fatalf("announced = %v, want the ended task once", announced)
	}
}
