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
