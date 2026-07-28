package sessions_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/sessions"
)

// taskRepo builds a repo and a task store over one database, which is how a
// host that has both wires them.
func taskRepo(t *testing.T) (*sessions.Repo, *sessions.TaskStore, *bun.DB) {
	t.Helper()
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "tasks.db")
	_, db, err := sessions.NewSQLite(dsn, "unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sessions.CreateSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	return sessions.NewRepo(db), sessions.NewTaskStore(db), db
}

func spawnedTask(t *testing.T, s *sessions.TaskStore, id, parent, child string) {
	t.Helper()
	if err := s.Create(context.Background(), &tasks.Task{
		ID: id, RunID: id + "-run", Label: "job " + id,
		ParentSessionID: parent, ChildSessionID: child,
		Depth: 1, Status: tasks.StatusWorking, NotifyState: tasks.NotifyPending,
	}); err != nil {
		t.Fatalf("create task %s: %v", id, err)
	}
}

// A task row belongs to one GENERATION of a session id. An id deleted and
// created again is a different session, and the replacement must not inherit
// the dead one's tasks — nor be woken for them.
func TestTaskRowsDoNotCrossSessionIncarnations(t *testing.T) {
	ctx := context.Background()
	repo, store, db := taskRepo(t)

	if _, err := repo.Create(ctx, agents.CreateOptions{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, agents.CreateOptions{ID: "child-1", Hidden: true}); err != nil {
		t.Fatal(err)
	}
	spawnedTask(t, store, "t1", "s1", "child-1")

	// The rows are visible to the incarnation that spawned them.
	if got, err := store.ListByParent(ctx, "s1"); err != nil || len(got) != 1 {
		t.Fatalf("before delete: %d tasks, err=%v; want 1", len(got), err)
	}

	if err := repo.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	// The cascade removed the row outright.
	var remaining int
	if err := db.NewSelect().Table("agent_tasks").
		ColumnExpr("count(*)").Where("id = ?", "t1").Scan(ctx, &remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Error("deleting a session left its task rows behind; their wake-up debt is retried at every restart, forever")
	}

	// The cascade is the first line; the generation columns are the second,
	// for a row that survives it — written concurrently with the delete, or
	// by a host that removes a session some other way. Recreate the id and
	// plant a row bound to the generation that is gone.
	if _, err := repo.Create(ctx, agents.CreateOptions{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewInsert().Table("agent_tasks").
		Model(&map[string]any{
			"id": "t3", "run_id": "t3-run", "label": "stale",
			"parent_session_id": "s1", "parent_session_gen": "a-dead-generation",
			"child_session_id": "child-1", "child_session_gen": "a-dead-generation",
			"depth": 1, "status": string(tasks.StatusWorking),
			"notify_state": string(tasks.NotifyPending),
			"created_at":   "2020-01-01 00:00:00+00:00", "updated_at": "2020-01-01 00:00:00+00:00",
		}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if got, err := store.ListByParent(ctx, "s1"); err != nil || len(got) != 0 {
		t.Fatalf("the recreated session inherited %d task(s) (err=%v); a task row is bound to one generation", len(got), err)
	}
	if got, err := store.ListPendingNotify(ctx, "s1"); err != nil || len(got) != 0 {
		t.Fatalf("the recreated session owes %d wake-up(s) it never asked for (err=%v)", len(got), err)
	}
	parents, err := store.PendingNotifyParents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parents {
		if p == "s1" {
			t.Fatal("the restart sweep would wake the replacement for a dead incarnation's task")
		}
	}
	if _, err := store.ByChildSession(ctx, "child-1"); err == nil {
		t.Fatal("a stale row resolved a child session it no longer owns")
	}
}

// The direct scope — sessions.New(db, id), which has no session row — stores
// the empty generation and keeps working: its tasks are visible to it.
func TestTaskRowsWorkWithoutASessionRow(t *testing.T) {
	ctx := context.Background()
	_, store, _ := taskRepo(t)

	spawnedTask(t, store, "t1", "direct-parent", "direct-child")

	if got, err := store.ListByParent(ctx, "direct-parent"); err != nil || len(got) != 1 {
		t.Fatalf("a session with no row lost its tasks: %d (err=%v)", len(got), err)
	}
	if _, err := store.ByChildSession(ctx, "direct-child"); err != nil {
		t.Fatalf("resolving a task by its child session with no row: %v", err)
	}
}
