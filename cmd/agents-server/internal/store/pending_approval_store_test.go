package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPendingApprovalRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewPendingApprovalStore(newTestDB(t))
	id := ids(t)

	calls, _ := json.Marshal([]PendingToolCall{{ToolCallID: "call-1", ToolName: "shell", Arguments: `{"cmd":"ls"}`}})
	p := &PendingApproval{
		RunID:         id("run-1"),
		SessionID:     id("sess-1"),
		AgentConfigID: id("agent-1"),
		State:         `{"schema_version":"1.0"}`,
		ToolCalls:     calls,
	}
	if err := s.Save(ctx, p); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Get and list.
	got, err := s.Get(ctx, id("run-1"))
	if err != nil || got.SessionID != id("sess-1") {
		t.Fatalf("get: %v / %+v", err, got)
	}
	bySession, err := s.ListBySession(ctx, id("sess-1"))
	if err != nil || len(bySession) != 1 {
		t.Fatalf("list by session: %v / %d", err, len(bySession))
	}

	// FindByToolCall locates the record and the specific call.
	found, tc, err := s.FindByToolCall(ctx, "call-1")
	if err != nil || found.RunID != id("run-1") || tc.ToolName != "shell" {
		t.Fatalf("find by tool call: %v / %+v / %+v", err, found, tc)
	}
	if _, _, err := s.FindByToolCall(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("find unknown call: want ErrNotFound, got %v", err)
	}

	// Save again upserts (no duplicate).
	p.State = `{"schema_version":"1.0","v":2}`
	if err := s.Save(ctx, p); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	all, _ := s.List(ctx)
	if len(all) != 1 {
		t.Fatalf("upsert produced %d rows, want 1", len(all))
	}

	// Delete claims the row; a second delete reports not-found so concurrent
	// decisions can use it as the exclusive claim.
	if err := s.Delete(ctx, id("run-1")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, id("run-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, id("run-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-delete: want ErrNotFound, got %v", err)
	}
}

func TestSessionDeleteCascadesPendingApprovals(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sessions := NewSessionStore(db)
	approvals := NewPendingApprovalStore(db)

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := approvals.Save(ctx, &PendingApproval{RunID: id("r1"), SessionID: sess.ID, State: "{}"}); err != nil {
		t.Fatal(err)
	}

	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := approvals.Get(ctx, id("r1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending approval should be cascade-deleted, got %v", err)
	}
}

func TestPendingApprovalReap(t *testing.T) {
	ctx := context.Background()
	s := NewPendingApprovalStore(newTestDB(t))
	id := ids(t)

	old := &PendingApproval{RunID: id("old"), SessionID: id("s"), State: "{}", CreatedAt: time.Now().UTC().Add(-2 * time.Hour)}
	fresh := &PendingApproval{RunID: id("fresh"), SessionID: id("s"), State: "{}"}
	if err := s.Save(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	// The listing names the expired candidates; it claims nothing.
	expired, err := s.ListOlderThan(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(expired) != 1 || expired[0].RunID != id("old") {
		t.Fatalf("expired = %+v, want the old one only", expired)
	}
	if all, _ := s.List(ctx); len(all) != 2 {
		t.Fatalf("listing must not remove anything: %d rows", len(all))
	}
}

// A paused task's approval is claimed and the task ended in ONE write — and
// of two racing claims exactly one takes the row: the loser writes nothing.
func TestClaimApprovalCancelledIsOneWriteAndExclusive(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	approvals, tasks := NewPendingApprovalStore(db), NewTaskStore(db)
	row := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: id("p"), ChildSessionID: id("c"), Status: "input_required", Label: "t"}
	if err := tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := approvals.Save(ctx, &PendingApproval{RunID: row.RunID, SessionID: id("c"), State: "{}"}); err != nil {
		t.Fatal(err)
	}
	// A pending debt of this attempt goes with the cancellation.
	if err := NewWakeupStore(db).Owe(ctx, &Wakeup{SessionID: id("p"), Kind: WakeKindTask, SourceID: row.ID, Attempt: row.RunID, Payload: "x"}); err != nil {
		t.Fatal(err)
	}
	type result struct{ claimed, ended bool }
	results := make(chan result, 2)
	for range 2 {
		go func() {
			c, e, err := tasks.ClaimApprovalCancelled(ctx, row.ID, row.RunID, "expired")
			if err != nil {
				t.Error(err)
			}
			results <- result{c, e}
		}()
	}
	var claims, ends int
	for range 2 {
		r := <-results
		if r.claimed {
			claims++
		}
		if r.ended {
			ends++
		}
	}
	if claims != 1 || ends != 1 {
		t.Fatalf("claims=%d ends=%d, want exactly one of each", claims, ends)
	}
	got, _ := tasks.Get(ctx, row.ID)
	if got.Status != "cancelled" || got.Summary != "expired" {
		t.Fatalf("task = %s %q, want cancelled by the claim", got.Status, got.Summary)
	}
	if _, err := approvals.Get(ctx, row.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the approval must be gone: %v", err)
	}
	pending, _ := NewWakeupStore(db).Pending(ctx, id("p"))
	if len(pending) != 0 {
		t.Fatalf("the debt of the cancelled attempt must be dropped: %+v", pending)
	}
	// A stale row — its task already on another attempt — is claimed and
	// removed, and the task untouched.
	row2 := &Task{ID: NewID(), RunID: id("new-run"), ParentSessionID: id("p"), ChildSessionID: id("c2"), Status: "working", Label: "t"}
	if err := tasks.Create(ctx, row2); err != nil {
		t.Fatal(err)
	}
	if err := approvals.Save(ctx, &PendingApproval{RunID: id("old-run"), SessionID: id("c2"), State: "{}"}); err != nil {
		t.Fatal(err)
	}
	claimed, ended, err := tasks.ClaimApprovalCancelled(ctx, row2.ID, id("old-run"), "expired")
	if err != nil || !claimed || ended {
		t.Fatalf("stale claim = %v,%v,%v — want claimed, not ended", claimed, ended, err)
	}
	if got, _ := tasks.Get(ctx, row2.ID); got.Status != "working" {
		t.Fatalf("a task on another attempt must not be touched: %s", got.Status)
	}
}

// ClaimApprovalWorking flips the task and removes the row together, or does
// neither: a task not paused on that run leaves the row where it is.
func TestClaimApprovalWorkingIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	approvals, tasks := NewPendingApprovalStore(db), NewTaskStore(db)
	row := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: id("p"), ChildSessionID: id("c"), Status: "working", Label: "t"}
	if err := tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := approvals.Save(ctx, &PendingApproval{RunID: row.RunID, SessionID: id("c"), State: "{}"}); err != nil {
		t.Fatal(err)
	}
	// Not paused yet: nothing written, the row still there.
	if out, err := tasks.ClaimApprovalWorking(ctx, row.ID, row.RunID); err != nil || out != ClaimTaskNotPaused {
		t.Fatalf("claim of an unpaused task = %v, %v — want ClaimTaskNotPaused", out, err)
	}
	if _, err := approvals.Get(ctx, row.RunID); err != nil {
		t.Fatalf("the row must survive a claim that did not hold: %v", err)
	}
	if err := tasks.MarkInputRequired(ctx, row.ID, row.RunID); err != nil {
		t.Fatal(err)
	}
	if out, err := tasks.ClaimApprovalWorking(ctx, row.ID, row.RunID); err != nil || out != ClaimWon {
		t.Fatalf("claim = %v, %v — want won", out, err)
	}
	if got, _ := tasks.Get(ctx, row.ID); got.Status != "working" {
		t.Fatalf("task = %s, want working again", got.Status)
	}
	if _, err := approvals.Get(ctx, row.RunID); !errors.Is(err, ErrNotFound) {
		t.Fatal("the row must be gone with the claim")
	}
	// A second decision finds the row taken.
	if out, _ := tasks.ClaimApprovalWorking(ctx, row.ID, row.RunID); out != ClaimTaken {
		t.Fatalf("second claim = %v, want taken", out)
	}
}

// TestListByParentTasks exercises the real join SQL: approvals inside a
// session's task child sessions come back tagged with their task, others
// (chat approvals, other parents' tasks) do not.
func TestListByParentTasks(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	s := NewPendingApprovalStore(db)
	tasks := NewTaskStore(db)

	mk := func(runID, sessionID string) {
		t.Helper()
		calls, _ := json.Marshal([]PendingToolCall{{ToolCallID: "call-" + runID, ToolName: "shell", Arguments: "{}"}})
		if err := s.Save(ctx, &PendingApproval{RunID: runID, SessionID: sessionID, State: "{}", ToolCalls: calls}); err != nil {
			t.Fatal(err)
		}
	}

	if err := tasks.Create(ctx, &Task{ID: id("task-1"), RunID: id("run-t1"), ParentSessionID: id("parent-1"), ChildSessionID: id("child-1"), Label: "mine", Status: "input_required"}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.Create(ctx, &Task{ID: id("task-2"), RunID: id("run-t2"), ParentSessionID: id("parent-2"), ChildSessionID: id("child-2"), Label: "other", Status: "input_required"}); err != nil {
		t.Fatal(err)
	}
	mk(id("run-t1"), id("child-1")) // parent-1's task approval
	mk(id("run-t2"), id("child-2")) // another parent's task approval
	mk(id("run-c"), id("parent-1")) // a plain chat approval in parent-1 itself

	got, err := s.ListByParentTasks(ctx, id("parent-1"))
	if err != nil {
		t.Fatalf("ListByParentTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d approvals, want exactly parent-1's task approval: %+v", len(got), got)
	}
	if got[0].RunID != id("run-t1") || got[0].TaskID != id("task-1") || got[0].TaskLabel != "mine" {
		t.Fatalf("joined row = %+v", got[0])
	}
}
