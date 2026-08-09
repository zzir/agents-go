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
	cfg := &SandboxConfig{ID: id, Name: id, Type: "ssh", Config: []byte(`{"addr":"h","user":"u","work_dir":"/srv"}`)}
	if err := NewSandboxStore(db).Create(context.Background(), cfg); err != nil {
		t.Fatalf("create sandbox config %s: %v", id, err)
	}
}

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

// The first sandbox-carrying run permanently binds (sandbox_id, work_dir):
// the binding is the session's file system context and must never change
// under a conversation that already touched it. Exactly one caller wins.
func TestBindSandboxIfEmptyKeepsTheFirstBinding(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	for _, id := range []string{"sb-1", "sb-2", "sb-3"} {
		createSandboxRow(t, db, id)
	}

	sess := &Session{ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, "sb-1", "/w1", 1)
	if err != nil || !won {
		t.Fatalf("first bind: won=%v err=%v, want a win", won, err)
	}
	won, err = sessions.BindSandboxIfEmpty(ctx, sess.ID, "sb-2", "/w2", 1)
	if err != nil || won {
		t.Fatalf("second bind: won=%v err=%v, want a silent loss", won, err)
	}
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SandboxID != "sb-1" || got.WorkDir != "/w1" {
		t.Fatalf("bound to (%q,%q), want the first binding (sb-1,/w1)", got.SandboxID, got.WorkDir)
	}

	// Binding nothing and binding a missing session are quiet non-events.
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, "", "/x", 1); err != nil || won {
		t.Fatalf("empty sandbox: won=%v err=%v, want a no-op", won, err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, NewID(), "sb-3", "", 1); err != nil || won {
		t.Fatalf("missing session: won=%v err=%v, want a no-op", won, err)
	}

	// An empty workDir is a valid binding value ("the sandbox's own default").
	other := &Session{ID: NewID(), Name: "o"}
	if err := sessions.Create(ctx, other); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, other.ID, "sb-1", "", 1); err != nil || !won {
		t.Fatalf("empty-workdir bind: won=%v err=%v, want a win", won, err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, other.ID, "sb-1", "/late", 1); err != nil || won {
		t.Fatalf("rebind after empty-workdir bind: won=%v err=%v, want a loss", won, err)
	}
}

// The reference count behind "may this cached instance be released": exact
// (config, workdir) pairs, dropping to zero when the last bound session goes.
func TestCountBindingRefs(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	createSandboxRow(t, db, "sb-1")
	createSandboxRow(t, db, "sb-2")

	mk := func(sandboxID, workDir string) *Session {
		s := &Session{ID: NewID(), Name: "s"}
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create: %v", err)
		}
		if sandboxID != "" {
			if won, err := sessions.BindSandboxIfEmpty(ctx, s.ID, sandboxID, workDir, 1); err != nil || !won {
				t.Fatalf("bind: won=%v err=%v", won, err)
			}
		}
		return s
	}
	a := mk("sb-1", "/w1")
	mk("sb-1", "/w2")
	mk("sb-2", "/w1")
	mk("", "")

	if n, err := sessions.CountBindingRefs(ctx, "sb-1", "/w1"); err != nil || n != 1 {
		t.Fatalf("CountBindingRefs(sb-1,/w1) = %d, %v; want 1", n, err)
	}
	if n, err := sessions.CountBindingRefs(ctx, "sb-none", ""); err != nil || n != 0 {
		t.Fatalf("CountBindingRefs(sb-none) = %d, %v; want 0", n, err)
	}
	if err := sessions.Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := sessions.CountBindingRefs(ctx, "sb-1", "/w1"); err != nil || n != 0 {
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

	sess := &Session{ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	// No sandbox config row at all — the delete already landed.
	won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, "sb-gone", "/w", 1)
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
	sessions := NewSessionStore(db)
	createSandboxRow(t, db, "sb-a")
	createSandboxRow(t, db, "sb-b")

	sess := &Session{ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}
	results := make(chan bool, 2)
	for i, wd := range []string{"/a", "/b"} {
		go func(sb, wd string) {
			won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, sb, wd, 1)
			if err != nil {
				t.Errorf("bind %s: %v", sb, err)
			}
			results <- won
		}("sb-"+wd[1:], wd)
		_ = i
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

// The bind CAS matches the revision its workdir was validated against: a
// config updated between the plan's read and the write makes the bind lose —
// the re-plan then validates against the new revision — instead of fixing a
// workdir vetted against values that no longer hold.
func TestBindSandboxRefusesAStaleRevision(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	sandboxes := NewSandboxStore(db)
	createSandboxRow(t, db, "sb-1")

	// The config moves to revision 2 after the caller read revision 1.
	up := &SandboxConfig{Name: "sb-1", Type: "ssh", Config: []byte(`{"addr":"h2","user":"u","work_dir":"/srv"}`)}
	if err := sandboxes.Update(ctx, "sb-1", up, 1, true); err != nil {
		t.Fatal(err)
	}

	sess := &Session{ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, "sb-1", "/w", 1); err != nil || won {
		t.Fatalf("stale-revision bind: won=%v err=%v, want a quiet loss", won, err)
	}
	if got, _ := sessions.Get(ctx, sess.ID); got.SandboxID != "" {
		t.Fatalf("session bound at a stale revision: %q", got.SandboxID)
	}
	// The re-read revision binds.
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, "sb-1", "/w", 2); err != nil || !won {
		t.Fatalf("current-revision bind: won=%v err=%v", won, err)
	}
}
