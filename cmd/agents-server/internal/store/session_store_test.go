package store

import (
	"context"
	"errors"
	"testing"
)

// deleting a task's hidden child session directly must remove the owning
// task row too, so no orphan tasks row is left pointing at a deleted session.
func TestSessionDeleteRemovesOwningTask(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{ID: NewID(), Name: "parent"}
	child := &Session{ID: NewID(), Name: "child"}
	for _, s := range []*Session{parent, child} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	task := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: child.ID, Status: "working"}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Delete the hidden child session directly (e.g. via the REST endpoint).
	if err := sessions.Delete(ctx, child.ID); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if _, err := tasks.Get(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owning task should be gone, got %v", err)
	}
}

// The first run to bind a session to an agent config wins: the binding is what
// a reload reopens the session with, and a later run under a different agent
// must not rewrite it. Binding one that is not there, or binding nothing, is
// not an error — the caller has a run to finish either way.
func TestBindAgentIfEmptyKeepsTheFirstBinding(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)

	sess := &Session{ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sessions.BindAgentIfEmpty(ctx, sess.ID, "agent-1"); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := sessions.BindAgentIfEmpty(ctx, sess.ID, "agent-2"); err != nil {
		t.Fatalf("second bind: %v", err)
	}
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentConfigID != "agent-1" {
		t.Fatalf("session bound to %q, want the first binding %q", got.AgentConfigID, "agent-1")
	}

	if err := sessions.BindAgentIfEmpty(ctx, sess.ID, ""); err != nil {
		t.Fatalf("binding nothing: %v", err)
	}
	if err := sessions.BindAgentIfEmpty(ctx, NewID(), "agent-3"); err != nil {
		t.Fatalf("binding a session that is not there: %v", err)
	}
}

// regression guard: deleting the parent still cascades to the task row and
// its hidden child session.
func TestSessionDeleteParentCascadesTask(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{ID: NewID(), Name: "parent"}
	child := &Session{ID: NewID(), Name: "child"}
	for _, s := range []*Session{parent, child} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	task := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: child.ID, Status: "working"}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := sessions.Delete(ctx, parent.ID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	if _, err := tasks.Get(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task should cascade with parent, got %v", err)
	}
	if _, err := sessions.Get(ctx, child.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden child session should cascade with parent, got %v", err)
	}
}
