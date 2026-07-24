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

	calls, _ := json.Marshal([]PendingToolCall{{ToolCallID: "call-1", ToolName: "shell", Arguments: `{"cmd":"ls"}`}})
	p := &PendingApproval{
		RunID:         "run-1",
		SessionID:     "sess-1",
		AgentConfigID: "agent-1",
		State:         `{"schema_version":"1.0"}`,
		ToolCalls:     calls,
	}
	if err := s.Save(ctx, p); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Get and list.
	got, err := s.Get(ctx, "run-1")
	if err != nil || got.SessionID != "sess-1" {
		t.Fatalf("get: %v / %+v", err, got)
	}
	bySession, err := s.ListBySession(ctx, "sess-1")
	if err != nil || len(bySession) != 1 {
		t.Fatalf("list by session: %v / %d", err, len(bySession))
	}

	// FindByToolCall locates the record and the specific call.
	found, tc, err := s.FindByToolCall(ctx, "call-1")
	if err != nil || found.RunID != "run-1" || tc.ToolName != "shell" {
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
	if err := s.Delete(ctx, "run-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "run-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "run-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-delete: want ErrNotFound, got %v", err)
	}
}

func TestSessionDeleteCascadesPendingApprovals(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	approvals := NewPendingApprovalStore(db)

	sess := &Session{ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := approvals.Save(ctx, &PendingApproval{RunID: "r1", SessionID: sess.ID, State: "{}"}); err != nil {
		t.Fatal(err)
	}

	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := approvals.Get(ctx, "r1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending approval should be cascade-deleted, got %v", err)
	}
}

func TestPendingApprovalReap(t *testing.T) {
	ctx := context.Background()
	s := NewPendingApprovalStore(newTestDB(t))

	old := &PendingApproval{RunID: "old", SessionID: "s", State: "{}", CreatedAt: time.Now().UTC().Add(-2 * time.Hour)}
	fresh := &PendingApproval{RunID: "fresh", SessionID: "s", State: "{}"}
	if err := s.Save(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	removed, err := s.DeleteOlderThan(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(removed) != 1 || removed[0].RunID != "old" {
		t.Fatalf("reap removed wrong set: %+v", removed)
	}
	if all, _ := s.List(ctx); len(all) != 1 || all[0].RunID != "fresh" {
		t.Fatalf("wrong survivor: %+v", all)
	}
}

// TestDeleteOlderThanReturnsOnlyDeleted locks: DeleteOlderThan reports
// EXACTLY the rows it removed. Two concurrent reaps over the same expired set
// must, between them, return each row exactly once — never the same row twice.
// The old SELECT-then-DELETE let both reaps SELECT the same rows before either
// DELETE, so both returned them, and the reaper then finalized a just-expired
// (or, in the approve race, just-resumed) task twice. DELETE... RETURNING makes
// the returned set the deleted set, so a row is reported by whichever statement
// actually removed it and by no other.
func TestDeleteOlderThanReturnsOnlyDeleted(t *testing.T) {
	ctx := context.Background()
	s := NewPendingApprovalStore(newTestDB(t))

	old := time.Now().UTC().Add(-2 * time.Hour)
	const n = 30
	for range n {
		if err := s.Save(ctx, &PendingApproval{RunID: NewID(), SessionID: "s", State: "{}", CreatedAt: old}); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := time.Now().UTC().Add(-time.Hour)
	type result struct {
		rows []PendingApproval
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			rows, err := s.DeleteOlderThan(ctx, cutoff)
			results <- result{rows, err}
		}()
	}

	seen := map[string]int{}
	total := 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatalf("DeleteOlderThan: %v", r.err)
		}
		for _, row := range r.rows {
			seen[row.RunID]++
			total++
		}
	}
	if total != n {
		t.Fatalf("two concurrent reaps returned %d rows total, want exactly %d (no row twice, none missed)", total, n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("row %s returned %d times; each expired row must be returned exactly once", id, c)
		}
	}
	// Everything is gone; a follow-up reap returns an empty set with no error.
	rest, err := s.DeleteOlderThan(ctx, cutoff)
	if err != nil || len(rest) != 0 {
		t.Fatalf("follow-up reap: rows=%d err=%v, want empty and nil", len(rest), err)
	}
}

// TestListByParentTasks exercises the real join SQL: approvals inside a
// session's task child sessions come back tagged with their task, others
// (chat approvals, other parents' tasks) do not.
func TestListByParentTasks(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewPendingApprovalStore(db)
	tasks := NewTaskStore(db)

	mk := func(runID, sessionID string) {
		t.Helper()
		calls, _ := json.Marshal([]PendingToolCall{{ToolCallID: "call-" + runID, ToolName: "shell", Arguments: "{}"}})
		if err := s.Save(ctx, &PendingApproval{RunID: runID, SessionID: sessionID, State: "{}", ToolCalls: calls}); err != nil {
			t.Fatal(err)
		}
	}

	if err := tasks.Create(ctx, &Task{ID: "task-1", RunID: "run-t1", ParentSessionID: "parent-1", ChildSessionID: "child-1", Label: "mine", Status: "input_required"}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.Create(ctx, &Task{ID: "task-2", RunID: "run-t2", ParentSessionID: "parent-2", ChildSessionID: "child-2", Label: "other", Status: "input_required"}); err != nil {
		t.Fatal(err)
	}
	mk("run-t1", "child-1") // parent-1's task approval
	mk("run-t2", "child-2") // another parent's task approval
	mk("run-c", "parent-1") // a plain chat approval in parent-1 itself

	got, err := s.ListByParentTasks(ctx, "parent-1")
	if err != nil {
		t.Fatalf("ListByParentTasks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d approvals, want exactly parent-1's task approval: %+v", len(got), got)
	}
	if got[0].RunID != "run-t1" || got[0].TaskID != "task-1" || got[0].TaskLabel != "mine" {
		t.Fatalf("joined row = %+v", got[0])
	}
}
