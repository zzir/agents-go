package store

import (
	"context"
	"testing"
)

// A task row belongs to one GENERATION of a session id (spec §2.13). The
// server mints random session ids, so an id is not reused in practice — but
// the guard is what makes that a property of the id generator rather than
// something the task queries depend on, and it is the same rule the sessions
// module enforces.
func TestTaskRowsAreBoundToASessionGeneration(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{ID: "s1", Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &Session{ID: "c1", Name: "task", Hidden: true}
	if err := sessions.Create(ctx, child); err != nil {
		t.Fatal(err)
	}

	task := &Task{
		ID: "t1", RunID: "r1", ParentSessionID: "s1", ChildSessionID: "c1",
		Status: taskWorking, NotifyState: NotifyPending,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	if got, err := tasks.ListByParent(ctx, "s1"); err != nil || len(got) != 1 {
		t.Fatalf("the spawning session sees %d task(s) (err=%v); want 1", len(got), err)
	}
	if _, err := tasks.ByChildSession(ctx, "c1"); err != nil {
		t.Fatalf("resolving the task by its child session: %v", err)
	}

	// Re-stamp both sessions with a new generation, which is what a delete and
	// a re-create under the same id amounts to as far as a surviving task row
	// is concerned.
	if _, err := db.NewUpdate().Model((*Session)(nil)).
		Set("gen = ?", "a-newer-generation").
		Where("id IN (?, ?)", "s1", "c1").Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if got, err := tasks.ListByParent(ctx, "s1"); err != nil || len(got) != 0 {
		t.Fatalf("the replacement session inherited %d task(s) (err=%v)", len(got), err)
	}
	if got, err := tasks.ListPendingNotify(ctx, "s1"); err != nil || len(got) != 0 {
		t.Fatalf("the replacement session owes %d wake-up(s) it never asked for (err=%v)", len(got), err)
	}
	parents, err := tasks.PendingNotifyParents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parents {
		if p == "s1" {
			t.Fatal("the restart sweep would wake the replacement for a dead incarnation's task")
		}
	}
	if _, err := tasks.ByChildSession(ctx, "c1"); err == nil {
		t.Fatal("a stale row resolved a child session it no longer owns")
	}
}

// Deleting a session takes its task rows with it, in both roles — the cascade
// is what stops a stale row existing at all.
func TestSessionDeleteCascadesTaskRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	for _, s := range []*Session{
		{ID: "s1", Name: "chat"},
		{ID: "c1", Name: "task", Hidden: true},
	} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := tasks.Create(ctx, &Task{
		ID: "t1", RunID: "r1", ParentSessionID: "s1", ChildSessionID: "c1",
		Status: taskWorking, NotifyState: NotifyPending,
	}); err != nil {
		t.Fatal(err)
	}

	if err := sessions.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}

	var remaining int
	if err := db.NewSelect().Model((*Task)(nil)).
		ColumnExpr("count(*)").Scan(ctx, &remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("%d task row(s) outlived their session; their wake-up debt is retried at every restart", remaining)
	}
	// The hidden child session went with it too — a transcript with no task
	// has no path in the UI at all.
	if _, err := sessions.Get(ctx, "c1"); err == nil {
		t.Error("the task's hidden child session outlived the delete cascade")
	}
}
