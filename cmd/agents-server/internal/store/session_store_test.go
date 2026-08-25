package store

import (
	"context"
	"errors"
	"testing"

	"github.com/uptrace/bun"
)

// createSandboxRow persists a sandbox config under the given id.
// BindSandboxIfEmpty refuses a bind whose target config is gone (the EXISTS
// predicate), so binding tests must create what they bind.
func createSandboxRow(t *testing.T, db *bun.DB, id string) {
	t.Helper()
	cfg := &SandboxConfig{ID: id, Name: id, Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@h"}`)}
	if err := NewSandboxStore(db).Create(context.Background(), cfg); err != nil {
		t.Fatalf("create sandbox config %s: %v", id, err)
	}
}

// createProjectRow persists a project on the given sandbox — a bind's target
// project must exist (the EXISTS guard mirrors ProjectStore.DeleteIfUnreferenced).
func createProjectRow(t *testing.T, db *bun.DB, id, sandboxID string) {
	t.Helper()
	p := &Project{ID: id, OwnerID: LocalUserID, SandboxID: sandboxID, Name: id}
	if err := NewProjectStore(db).Create(context.Background(), p); err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
}

// deleting a task's hidden child session directly must remove the owning
// task row too, so no orphan tasks row is left pointing at a deleted session.
func TestSessionDeleteRemovesOwningTask(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "parent"}
	child := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "child"}
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
	id := ids(t)
	sessions := NewSessionStore(db)

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sessions.BindAgentIfEmpty(ctx, sess.ID, id("agent-1")); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := sessions.BindAgentIfEmpty(ctx, sess.ID, id("agent-2")); err != nil {
		t.Fatalf("second bind: %v", err)
	}
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentConfigID != id("agent-1") {
		t.Fatalf("session bound to %q, want the first binding %q", got.AgentConfigID, id("agent-1"))
	}

	if err := sessions.BindAgentIfEmpty(ctx, sess.ID, ""); err != nil {
		t.Fatalf("binding nothing: %v", err)
	}
	if err := sessions.BindAgentIfEmpty(ctx, NewID(), id("agent-3")); err != nil {
		t.Fatalf("binding a session that is not there: %v", err)
	}
}

// The first sandbox-carrying run permanently binds (sandbox_id, work_dir):
// the binding is the session's file system context and must never change
// under a conversation that already touched it. Exactly one caller wins.
func TestBindSandboxIfEmptyKeepsTheFirstBinding(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sessions := NewSessionStore(db)
	for _, name := range []string{"sb-1", "sb-2", "sb-3"} {
		createSandboxRow(t, db, id(name))
	}

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	createProjectRow(t, db, id("p-1"), id("sb-1"))
	createProjectRow(t, db, id("p-2"), id("sb-2"))
	won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, id("sb-1"), id("p-1"), 1)
	if err != nil || !won {
		t.Fatalf("first bind: won=%v err=%v, want a win", won, err)
	}
	won, err = sessions.BindSandboxIfEmpty(ctx, sess.ID, id("sb-2"), id("p-2"), 1)
	if err != nil || won {
		t.Fatalf("second bind: won=%v err=%v, want a silent loss", won, err)
	}
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SandboxID != id("sb-1") || got.ProjectID != id("p-1") {
		t.Fatalf("bound to (%q,%q), want the first binding (sb-1,p-1)", got.SandboxID, got.ProjectID)
	}

	// Binding nothing and binding a missing session are quiet non-events.
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, "", id("p-1"), 1); err != nil || won {
		t.Fatalf("empty sandbox: won=%v err=%v, want a no-op", won, err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, NewID(), id("sb-1"), id("p-1"), 1); err != nil || won {
		t.Fatalf("missing session: won=%v err=%v, want a no-op", won, err)
	}

	// A vanished PROJECT loses the same way a vanished config does.
	other := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "o"}
	if err := sessions.Create(ctx, other); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, other.ID, id("sb-1"), NewID(), 1); err != nil || won {
		t.Fatalf("vanished-project bind: won=%v err=%v, want a quiet loss", won, err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, other.ID, id("sb-1"), id("p-1"), 1); err != nil || !won {
		t.Fatalf("valid bind after the loss: won=%v err=%v, want a win", won, err)
	}
}

// The reference count behind "may this cached instance be released": exact
// (config, project) pairs, dropping to zero when the last bound session goes.
func TestCountBindingRefs(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sessions := NewSessionStore(db)
	createSandboxRow(t, db, id("sb-1"))
	createSandboxRow(t, db, id("sb-2"))

	createProjectRow(t, db, id("p-1"), id("sb-1"))
	createProjectRow(t, db, id("p-2"), id("sb-1"))
	createProjectRow(t, db, id("p-3"), id("sb-2"))
	mk := func(sandboxID, projectID string) *Session {
		s := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create: %v", err)
		}
		if sandboxID != "" {
			if won, err := sessions.BindSandboxIfEmpty(ctx, s.ID, sandboxID, projectID, 1); err != nil || !won {
				t.Fatalf("bind: won=%v err=%v", won, err)
			}
		}
		return s
	}
	a := mk(id("sb-1"), id("p-1"))
	mk(id("sb-1"), id("p-2"))
	mk(id("sb-2"), id("p-3"))
	mk("", "")

	if n, err := sessions.CountBindingRefs(ctx, id("sb-1"), id("p-1")); err != nil || n != 1 {
		t.Fatalf("CountBindingRefs(sb-1,p-1) = %d, %v; want 1", n, err)
	}
	if n, err := sessions.CountBindingRefs(ctx, NewID(), ""); err != nil || n != 0 {
		t.Fatalf("CountBindingRefs(sb-none) = %d, %v; want 0", n, err)
	}
	// An unset half is stored as NULL, so it is asked for as NULL: the pair
	// ("", "") is the unbound sessions, not a syntax error (PostgreSQL refuses
	// "" as a uuid) and not silently nothing (what SQLite would answer).
	if n, err := sessions.CountBindingRefs(ctx, "", ""); err != nil || n != 1 {
		t.Fatalf("CountBindingRefs(unbound) = %d, %v; want the one unbound session", n, err)
	}
	if err := sessions.Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := sessions.CountBindingRefs(ctx, id("sb-1"), id("p-1")); err != nil || n != 0 {
		t.Fatalf("CountBindingRefs after delete = %d, %v; want 0", n, err)
	}
}

// The EXISTS predicate: a bind whose target config vanished between the
// caller's validation and the write must lose, not point the session at
// nothing forever — the caller's re-plan then reports the config as missing.
func TestBindSandboxRefusesVanishedTarget(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	// No sandbox config row at all — the delete already landed.
	won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, NewID(), NewID(), 1)
	if err != nil || won {
		t.Fatalf("bind to a vanished config: won=%v err=%v, want a quiet loss", won, err)
	}
	if got, _ := sessions.Get(ctx, sess.ID); got.SandboxID != "" {
		t.Fatalf("session bound to %q, want unbound", got.SandboxID)
	}
}

// Two racing binds settle in SQL: exactly one wins.
func TestBindSandboxIfEmptyRace(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sessions := NewSessionStore(db)
	createSandboxRow(t, db, id("sb-a"))
	createSandboxRow(t, db, id("sb-b"))
	createProjectRow(t, db, id("p-a"), id("sb-a"))
	createProjectRow(t, db, id("p-b"), id("sb-b"))

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	results := make(chan bool, 2)
	for _, n := range []string{"a", "b"} {
		go func(sb, pj string) {
			won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, sb, pj, 1)
			if err != nil {
				t.Errorf("bind %s: %v", sb, err)
			}
			results <- won
		}(id("sb-"+n), id("p-"+n))
	}
	wins := 0
	for range 2 {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
}

// regression guard: deleting the parent still cascades to the task row and
// its hidden child session.
func TestSessionDeleteParentCascadesTask(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "parent"}
	child := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "child"}
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

// The bind CAS matches the revision its plan was validated against: a
// config updated between the plan's read and the write makes the bind lose —
// the re-plan then validates against the new revision — instead of fixing a
// workdir vetted against values that no longer hold.
func TestBindSandboxRefusesAStaleRevision(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sessions := NewSessionStore(db)
	sandboxes := NewSandboxStore(db)
	createSandboxRow(t, db, id("sb-1"))
	createProjectRow(t, db, id("p-1"), id("sb-1"))

	// The config moves to revision 2 after the caller read revision 1.
	up := &SandboxConfig{Name: "sb-1", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@h2"}`)}
	if err := sandboxes.Update(ctx, id("sb-1"), up, 1, true); err != nil {
		t.Fatal(err)
	}

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, id("sb-1"), id("p-1"), 1); err != nil || won {
		t.Fatalf("stale-revision bind: won=%v err=%v, want a quiet loss", won, err)
	}
	if got, _ := sessions.Get(ctx, sess.ID); got.SandboxID != "" {
		t.Fatalf("session bound at a stale revision: %q", got.SandboxID)
	}
	// The re-read revision binds.
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, id("sb-1"), id("p-1"), 2); err != nil || !won {
		t.Fatalf("current-revision bind: won=%v err=%v", won, err)
	}
}

// The title generator names a session only while it still carries the default
// name: a rename made while it was thinking stands.
func TestNameIfDefaultKeepsAPersonsRename(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: DefaultSessionName}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.NameIfDefault(ctx, sess.ID, "Sorting a list"); err != nil || !won {
		t.Fatalf("first naming = %v, %v — want it to land", won, err)
	}
	// A second generator, or one arriving after a rename, loses.
	if won, err := sessions.NameIfDefault(ctx, sess.ID, "Something else"); err != nil || won {
		t.Fatalf("second naming = %v, %v — want it to lose", won, err)
	}
	if err := sessions.Update(ctx, sess.ID, "My name"); err != nil {
		t.Fatal(err)
	}
	if won, _ := sessions.NameIfDefault(ctx, sess.ID, "Generated"); won {
		t.Fatal("a generated title must not overwrite a person's name")
	}
	got, _ := sessions.Get(ctx, sess.ID)
	if got.Name != "My name" {
		t.Fatalf("name = %q, want the person's", got.Name)
	}
}

// Delete follows the task tree to any depth — a workflow step's task under a
// hidden session goes with the conversation — but only over LIVE edges: a
// stale task row from an earlier incarnation of the parent names a child id
// that may since have been reused by an unrelated session, and following it
// would delete that session's history.
func TestSessionDeleteCascadesTheWholeTreeOverLiveEdges(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	root := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "root"}
	child := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "child", Hidden: true}
	grandchild := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "grandchild", Hidden: true}
	// A session that shares an id with the root's FORMER incarnation's child:
	// the stale row below points at it, but was bound to a generation of the
	// root that no longer exists.
	bystander := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "bystander"}
	for _, s := range []*Session{root, child, grandchild, bystander} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	for _, task := range []*Task{
		{ID: NewID(), RunID: NewID(), ParentSessionID: root.ID, ChildSessionID: child.ID, Status: "completed"},
		{ID: NewID(), RunID: NewID(), ParentSessionID: child.ID, ChildSessionID: grandchild.ID, Status: "completed"},
	} {
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}
	// The stale edge: root → bystander, bound to a generation the root never
	// had (what a row left over from a deleted-and-recreated root looks like).
	stale := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: root.ID, ChildSessionID: bystander.ID, Status: "completed"}
	if _, err := db.NewInsert().Model(stale).
		Value("parent_session_gen", "?", "gen-of-a-former-root").
		Value("child_session_gen", genOf, bystander.ID).
		Exec(ctx); err != nil {
		t.Fatalf("insert stale task: %v", err)
	}

	if err := sessions.Delete(ctx, root.ID); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		if _, err := sessions.Get(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session %s should be gone with the tree, got %v", id, err)
		}
	}
	if _, err := sessions.Get(ctx, bystander.ID); err != nil {
		t.Fatalf("the bystander behind a stale edge must survive: %v", err)
	}
	// The stale row itself goes — it named the deleted root as its parent.
	if _, err := tasks.Get(ctx, stale.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale task row should be deleted with its parent, got %v", err)
	}
	if err := sessions.Delete(ctx, root.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
}

// Reassigning a session moves the hidden sessions serving it too — the tree
// has one owner — over live task edges only, and a missing root is not found.
func TestSetOwnerMovesTheWholeTree(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	root := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "root"}
	child := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "child", Hidden: true}
	grandchild := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "grandchild", Hidden: true}
	bystander := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "bystander"}
	for _, s := range []*Session{root, child, grandchild, bystander} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	for _, task := range []*Task{
		{ID: NewID(), RunID: NewID(), ParentSessionID: root.ID, ChildSessionID: child.ID, Status: "completed"},
		{ID: NewID(), RunID: NewID(), ParentSessionID: child.ID, ChildSessionID: grandchild.ID, Status: "completed"},
	} {
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}
	stale := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: root.ID, ChildSessionID: bystander.ID, Status: "completed"}
	if _, err := db.NewInsert().Model(stale).
		Value("parent_session_gen", "?", "gen-of-a-former-root").
		Value("child_session_gen", genOf, bystander.ID).
		Exec(ctx); err != nil {
		t.Fatalf("insert stale task: %v", err)
	}

	alice := &User{Email: "alice@example.com", Role: RoleMember}
	if _, err := db.NewInsert().Model(alice).Exec(ctx); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := sessions.SetOwner(ctx, root.ID, alice.ID); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		if got, err := sessions.Get(ctx, id); err != nil || got.OwnerID != alice.ID {
			t.Fatalf("session %s owner = %v, %v; want %s", id, got, err, alice.ID)
		}
	}
	if got, _ := sessions.Get(ctx, bystander.ID); got.OwnerID != LocalUserID {
		t.Fatalf("the bystander behind a stale edge moved: owner = %s", got.OwnerID)
	}
	if err := sessions.SetOwner(ctx, NewID(), alice.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reassigning a missing session: want ErrNotFound, got %v", err)
	}
}
