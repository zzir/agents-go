package sessions_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
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
		Depth: 1, Status: tasks.StatusWorking,
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

	if _, err := repo.Create(ctx, session.CreateOptions{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, session.CreateOptions{ID: "child-1", Hidden: true}); err != nil {
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
	if _, err := repo.Create(ctx, session.CreateOptions{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewInsert().Table("agent_tasks").
		Model(&map[string]any{
			"id": "t3", "run_id": "t3-run", "label": "stale",
			"parent_session_id": "s1", "parent_session_gen": "a-dead-generation",
			"child_session_id": "child-1", "child_session_gen": "a-dead-generation",
			"depth": 1, "status": string(tasks.StatusWorking),
			"created_at": "2020-01-01 00:00:00+00:00", "updated_at": "2020-01-01 00:00:00+00:00",
		}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if got, err := store.ListByParent(ctx, "s1"); err != nil || len(got) != 0 {
		t.Fatalf("the recreated session inherited %d task(s) (err=%v); a task row is bound to one generation", len(got), err)
	}
	if _, err := store.ByChildSession(ctx, "child-1"); err == nil {
		t.Fatal("a stale row resolved a child session it no longer owns")
	}

	// The restart sweep obeys the same rule: a live working row is failed AND
	// reported, while the dead-generation row is neither — reporting it would
	// tell the replacement session of work it never spawned.
	spawnedTask(t, store, "t4", "s1", "child-4")
	orphans, err := store.FailOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].ID != "t4" {
		t.Fatalf("sweep reported %+v, want exactly the live orphan t4", orphans)
	}
	if orphans[0].Status != tasks.StatusFailed || orphans[0].Summary == "" {
		t.Fatalf("reported orphan does not carry its failure: %+v", orphans[0])
	}
	var status string
	if err := db.NewSelect().Table("agent_tasks").Column("status").
		Where("id = ?", "t3").Scan(ctx, &status); err != nil {
		t.Fatal(err)
	}
	if status != string(tasks.StatusWorking) {
		t.Fatalf("the sweep wrote to a dead-generation row (status %q)", status)
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

// Deleting a session takes the whole task tree with it: the hidden sessions its
// tasks ran in (and theirs, at any depth) go too — a hidden session has no
// listing of its own, so anything left behind would be unreachable forever.
func TestDeleteRemovesTheTaskTree(t *testing.T) {
	ctx := context.Background()
	repo, store, _ := taskRepo(t)

	for _, id := range []string{"root", "child-1", "grandchild-1", "unrelated"} {
		if _, err := repo.Create(ctx, session.CreateOptions{ID: id, Hidden: id != "root" && id != "unrelated"}); err != nil {
			t.Fatal(err)
		}
	}
	spawnedTask(t, store, "t1", "root", "child-1")
	spawnedTask(t, store, "t2", "child-1", "grandchild-1")
	// The child transcripts have content, which must go with them.
	for _, id := range []string{"child-1", "grandchild-1"} {
		s, err := repo.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendItems(ctx, agents.InputItemsFromText("hi"), agents.Source{}); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.Delete(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"child-1", "grandchild-1"} {
		if _, err := repo.Open(ctx, id); err == nil {
			t.Errorf("hidden session %s survived its owner's delete", id)
		}
	}
	for _, id := range []string{"t1", "t2"} {
		if _, err := store.Get(ctx, id); err == nil {
			t.Errorf("task %s survived its tree's delete", id)
		}
	}
	// A session outside the tree is untouched.
	if _, err := repo.Open(ctx, "unrelated"); err != nil {
		t.Fatalf("an unrelated session was deleted: %v", err)
	}
}

// The cascade follows LIVE edges only: a stale task row — its child id since
// given to an unrelated session (a new generation) — must not take that
// session with it. Same fence as every read: a stale row is inert, not wrong.
func TestDeleteFollowsLiveEdgesOnly(t *testing.T) {
	ctx := context.Background()
	repo, _, db := taskRepo(t)
	for _, id := range []string{"root", "reused"} {
		if _, err := repo.Create(ctx, session.CreateOptions{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	// A stale row: root → reused, bound to a generation of the child that
	// is not the one answering to the id now (what a row of a deleted and
	// re-created child looks like — the cascade of that delete would have
	// taken a row it could see, so the stale one is inserted directly).
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_tasks (id, run_id, label, parent_session_id, parent_session_gen, child_session_id, child_session_gen, depth, status, created_at, updated_at)
		VALUES ('t-old', 't-old-run', 'old', 'root', COALESCE((SELECT s.gen FROM agent_sessions AS s WHERE s.id = 'root'), ''), 'reused', 'gen-of-a-former-child', 1, ?, ?, ?)`,
		string(tasks.StatusWorking), time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	fresh, err := repo.Open(ctx, "reused")
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.AppendItems(ctx, agents.InputItemsFromText("someone else's history"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	// Deleting the old parent must not reach the reused id: the edge is stale.
	if err := repo.Delete(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	again, err := repo.Open(ctx, "reused")
	if err != nil {
		t.Fatalf("the unrelated session that reused the id was deleted: %v", err)
	}
	items, err := again.Entries(ctx, session.Cursor{})
	if err != nil || len(items) == 0 {
		t.Fatalf("the reused session lost its history: %d entries, %v", len(items), err)
	}
}
