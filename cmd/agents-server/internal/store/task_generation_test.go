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

	parent := &Session{OwnerID: LocalUserID, ID: "s1", Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &Session{OwnerID: LocalUserID, ID: "c1", Name: "task", Hidden: true}
	if err := sessions.Create(ctx, child); err != nil {
		t.Fatal(err)
	}

	task := &Task{
		ID: "t1", RunID: "r1", ParentSessionID: "s1", ChildSessionID: "c1",
		Status: taskWorking,
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
}

// Deleting a session takes its task rows with it, in both roles — the cascade
// is what stops a stale row existing at all.
func TestSessionDeleteCascadesTaskRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	for _, s := range []*Session{
		{ID: "s1", OwnerID: LocalUserID, Name: "chat"},
		{ID: "c1", OwnerID: LocalUserID, Name: "task", Hidden: true},
	} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := tasks.Create(ctx, &Task{
		ID: "t1", RunID: "r1", ParentSessionID: "s1", ChildSessionID: "c1",
		Status: taskWorking,
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

// ListRecent spans sessions: newest first, each row naming its conversation,
// narrowed by kind and to live rows on request — and, like every by-session
// read, blind to a dead incarnation's rows.
func TestListRecentSpansSessions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	a := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "alpha"}
	b := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "beta"}
	for _, s := range []*Session{a, b} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(parent *Session, kind, status string) *Task {
		child := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "child", Hidden: true}
		if err := sessions.Create(ctx, child); err != nil {
			t.Fatal(err)
		}
		task := &Task{ID: NewID(), RunID: NewID(), Kind: kind, ParentSessionID: parent.ID, ChildSessionID: child.ID, Status: status}
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	plain := mk(a, "", "working")
	wfDone := mk(a, TaskKindWorkflow, "completed")
	wfLive := mk(b, TaskKindWorkflow, "working")
	// A row of a former incarnation of alpha: bound to a generation that is
	// not the one answering to the id now.
	stale := &Task{ID: NewID(), RunID: NewID(), Kind: TaskKindWorkflow, ParentSessionID: a.ID, ChildSessionID: NewID(), Status: "working"}
	if _, err := db.NewInsert().Model(stale).
		Value("parent_session_gen", "?", "gen-of-a-former-alpha").
		Value("child_session_gen", "?", "").
		Exec(ctx); err != nil {
		t.Fatal(err)
	}

	all, total, err := tasks.ListRecent(ctx, "", "", false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || total != 3 {
		t.Fatalf("all = %d rows of %d, want 3 (the stale row must not list)", len(all), total)
	}
	names := map[string]string{}
	for _, r := range all {
		names[r.ID] = r.SessionName
	}
	if names[plain.ID] != "alpha" || names[wfDone.ID] != "alpha" || names[wfLive.ID] != "beta" {
		t.Fatalf("session names = %v", names)
	}

	wfs, total, err := tasks.ListRecent(ctx, "", TaskKindWorkflow, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(wfs) != 2 || total != 2 {
		t.Fatalf("workflows = %d rows of %d, want 2", len(wfs), total)
	}
	live, _, err := tasks.ListRecent(ctx, "", TaskKindWorkflow, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != wfLive.ID {
		t.Fatalf("live workflows = %+v, want just %s", live, wfLive.ID)
	}
	// A page: one row at a time, the total unchanged, the second page the
	// next row.
	first, total, err := tasks.ListRecent(ctx, "", "", false, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := tasks.ListRecent(ctx, "", "", false, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || total != 3 || first[0].ID == second[0].ID {
		t.Fatalf("paging: first %v second %v total %d", first, second, total)
	}
}
