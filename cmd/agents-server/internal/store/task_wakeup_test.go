package store

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/tasks"
)

// A completed task's wake-up is written IN THE SAME transaction as its terminal
// state, through the adapter — so a reader can never find a task done with its
// parent owed nothing. A cancellation writes no debt; the person did it.
func TestFinalizeWritesTheWakeupAtomically(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	ts := NewTaskStore(db)
	adapter := NewTaskAdapter(ts)
	wakeups := NewWakeupStore(db)

	mk := func(label string) *Task {
		row := &Task{
			ID: NewID(), RunID: NewID(), ParentSessionID: NewID(), ChildSessionID: NewID(),
			Label: label, Status: "working", ParentRunID: id("asker"), ParentAgentConfigID: id("ac"),
		}
		if err := ts.Create(ctx, row); err != nil {
			t.Fatal(err)
		}
		return row
	}

	// Completed → one pending wake-up, addressed to the parent, carrying the
	// notification the model will read.
	done := mk("audit")
	if won, err := adapter.Finalize(ctx, done.ID, done.RunID, tasks.StatusCompleted, "all green", "the full report", nil); err != nil || !won {
		t.Fatalf("Finalize won=%v err=%v", won, err)
	}
	pending, err := wakeups.Pending(ctx, done.ParentSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d wake-ups after a completed task, want 1", len(pending))
	}
	if w := pending[0]; w.Kind != WakeKindTask || w.SourceID != done.ID || w.ParentRunID != id("asker") ||
		!strings.Contains(w.Payload, "audit") || !strings.HasPrefix(w.Payload, tasks.NotificationPrefix) {
		t.Fatalf("wake-up = %+v, want a task debt addressed to the parent with the notification payload", w)
	}

	// Cancelled → no debt.
	cancelled := mk("scratch")
	if won, err := adapter.Finalize(ctx, cancelled.ID, cancelled.RunID, tasks.StatusCancelled, "stopped", "", nil); err != nil || !won {
		t.Fatalf("Finalize(cancelled) won=%v err=%v", won, err)
	}
	if p, err := wakeups.Pending(ctx, cancelled.ParentSessionID); err != nil || len(p) != 0 {
		t.Fatalf("cancelled task owed %d wake-ups (err=%v), want 0", len(p), err)
	}

	// A superseded attempt (wrong run id) changes nothing — no status move, no
	// debt.
	other := mk("stale")
	if won, err := adapter.Finalize(ctx, other.ID, NewID(), tasks.StatusCompleted, "x", "", nil); err != nil || won {
		t.Fatalf("Finalize(stale run) won=%v err=%v, want won=false", won, err)
	}
	if p, err := wakeups.Pending(ctx, other.ParentSessionID); err != nil || len(p) != 0 {
		t.Fatalf("a lost CAS owed %d wake-ups (err=%v), want 0", len(p), err)
	}
}

// FailOrphans fails restart-orphaned tasks AND records each one's wake-up in the
// same transaction, so the sweep cannot fail a task and forget its parent.
func TestFailOrphansWritesWakeups(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	ts := NewTaskStore(db)
	adapter := NewTaskAdapter(ts)
	wakeups := NewWakeupStore(db)

	orphan := &Task{
		ID: NewID(), RunID: NewID(), ParentSessionID: NewID(), ChildSessionID: NewID(),
		Label: "left running", Status: "working", ParentAgentConfigID: id("ac"),
	}
	if err := ts.Create(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	failed, err := adapter.FailOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].Status != tasks.StatusFailed {
		t.Fatalf("FailOrphans = %+v, want one failed task", failed)
	}
	p, err := wakeups.Pending(ctx, orphan.ParentSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 || !strings.Contains(p[0].Payload, "left running") {
		t.Fatalf("orphan wake-ups = %+v, want one naming the task", p)
	}
}

// A retry cancels the prior attempt's pending failure debt (so a busy parent is
// not told the old failure while the new attempt runs); a retry whose launch
// then fails owes a FRESH failure debt (the prior one is gone).
func TestRetryWakeupLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	ts := NewTaskStore(db)
	adapter := NewTaskAdapter(ts)
	wakeups := NewWakeupStore(db)

	task := &Task{ID: NewID(), RunID: id("runA"), ParentSessionID: NewID(), ChildSessionID: NewID(),
		Label: "audit", Status: "working", ParentAgentConfigID: id("ac")}
	if err := ts.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	// Attempt A fails → one pending debt.
	if won, err := adapter.Finalize(ctx, task.ID, id("runA"), tasks.StatusFailed, "boom", "", nil); err != nil || !won {
		t.Fatalf("finalize A: won=%v err=%v", won, err)
	}
	if p, _ := wakeups.Pending(ctx, task.ParentSessionID); len(p) != 1 {
		t.Fatalf("after A fails: %d pending, want 1", len(p))
	}

	// Retry claims attempt B → A's debt is cancelled in the same transition.
	if won, err := ts.RetryClaim(ctx, task.ID, id("runB"), 5); err != nil || !won {
		t.Fatalf("retry claim: won=%v err=%v", won, err)
	}
	if p, _ := wakeups.Pending(ctx, task.ParentSessionID); len(p) != 0 {
		t.Fatalf("after retry claim: %d pending, want 0 (the stale failure debt is cancelled)", len(p))
	}

	// The retry's launch fails → release owes a fresh failure debt.
	if won, err := adapter.ReleaseRetryClaim(ctx, task.ID, id("runB"), "launch failed", ""); err != nil || !won {
		t.Fatalf("release: won=%v err=%v", won, err)
	}
	p, _ := wakeups.Pending(ctx, task.ParentSessionID)
	if len(p) != 1 || p[0].Attempt != id("runB") {
		t.Fatalf("after release: pending=%+v, want one fresh debt for runB", p)
	}
}
