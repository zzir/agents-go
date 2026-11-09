package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Create runs behind locks on the target AND the template rows: a missing
// either refuses the insert instead of leaving a workspace row that points at
// nothing (decisions §5.28).
func TestWorkspaceCreateRequiresSandbox(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	workspaces := NewWorkspaceStore(db)

	p := &Workspace{ID: NewID(), OwnerID: LocalUserID, SandboxID: NewID(), Name: "p"}
	if err := workspaces.Create(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create on a missing sandbox: err=%v, want ErrNotFound", err)
	}

	createSandboxRow(t, db, id("sb-1"))
	p = &Workspace{ID: NewID(), OwnerID: LocalUserID, SandboxID: id("sb-1"), Name: "p"}
	if err := workspaces.Create(ctx, p); err != nil {
		t.Fatalf("create on an existing sandbox: %v", err)
	}
}

// List scopes to one owner — EveryOwner is the admin listing across all —
// and each row carries its bound-session count.
func TestWorkspaceListScopeAndSessionCount(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	workspaces := NewWorkspaceStore(db)

	createSandboxRow(t, db, id("tg"))
	mine := &Workspace{ID: NewID(), OwnerID: LocalUserID, SandboxID: id("tg"), Name: "mine"}
	foreign := &Workspace{ID: NewID(), OwnerID: NewID(), SandboxID: id("tg"), Name: "foreign"}
	for _, p := range []*Workspace{mine, foreign} {
		if err := workspaces.Create(ctx, p); err != nil {
			t.Fatalf("create workspace %s: %v", p.Name, err)
		}
	}
	s := &Session{ID: NewID(), OwnerID: LocalUserID, Name: "s", WorkspaceID: mine.ID}
	if err := NewSessionStore(db).Create(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	own, err := workspaces.List(ctx, LocalUserID)
	if err != nil {
		t.Fatalf("list own: %v", err)
	}
	if len(own) != 1 || own[0].ID != mine.ID {
		t.Fatalf("own listing = %+v, want just %s", own, mine.ID)
	}
	if own[0].SessionCount != 1 {
		t.Fatalf("session count = %d, want 1", own[0].SessionCount)
	}

	all, err := workspaces.List(ctx, EveryOwner)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("EveryOwner listing has %d rows, want 2", len(all))
	}
}

// Create builds its own transaction (it locks the target and template rows), so it bypasses
// the CrudStore write path that seals — the seal has to be applied there too,
// or the one path that CREATES an environment writes it in the clear.
func TestWorkspaceEnvSealedOnCreate(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	withTestBox(t)
	workspaces := NewWorkspaceStore(db)
	createSandboxRow(t, db, id("tg"))

	env, err := NormalizeWorkspaceEnv([]EnvVar{{Key: "TOKEN", Value: "sk-live"}, {Key: "TZ", Value: "UTC"}})
	if err != nil {
		t.Fatal(err)
	}
	p := &Workspace{ID: NewID(), OwnerID: LocalUserID, SandboxID: id("tg"), Name: "p", Env: env}
	if err := workspaces.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.Env != env {
		t.Errorf("caller's env after Create = %q, want plaintext", p.Env)
	}
	// Every value, not a chosen few: the environment is write-only
	// (decisions §5.32).
	raw := rawColumn(t, db, "SELECT env FROM workspaces WHERE id = ?", p.ID)
	if strings.Contains(raw, "sk-live") || strings.Contains(raw, "UTC") {
		t.Errorf("env at rest = %q, want every value sealed", raw)
	}
	got, err := workspaces.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Env != env {
		t.Errorf("Get env = %q, want the plaintext %q", got.Env, env)
	}
	if got.Revision != 1 || got.RuntimeGen != 1 {
		t.Errorf("counters after create = %d/%d, want 1/1", got.Revision, got.RuntimeGen)
	}
}

// The update is a compare-and-set, and only a CONTENT change moves the
// runtime generation — a rename must not replace anyone's container.
func TestWorkspaceUpdateCASAndGenerations(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	withTestBox(t)
	workspaces := NewWorkspaceStore(db)
	createSandboxRow(t, db, id("tg"))

	p := &Workspace{ID: NewID(), OwnerID: LocalUserID, SandboxID: id("tg"), Name: "p"}
	if err := workspaces.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	renamed := *p
	renamed.Name = "renamed"
	if _, err := workspaces.Update(ctx, p.ID, &renamed, 1, false); err != nil {
		t.Fatal(err)
	}
	got, err := workspaces.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || got.Revision != 2 || got.RuntimeGen != 1 {
		t.Errorf("after rename: name=%s rev=%d gen=%d, want renamed/2/1", got.Name, got.Revision, got.RuntimeGen)
	}

	env, err := NormalizeWorkspaceEnv([]EnvVar{{Key: "TOKEN", Value: "sk-live"}})
	if err != nil {
		t.Fatal(err)
	}
	withEnv := *got
	withEnv.Env = env
	if _, err := workspaces.Update(ctx, p.ID, &withEnv, 2, true); err != nil {
		t.Fatal(err)
	}
	if got, err = workspaces.Get(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got.Env != env || got.Revision != 3 || got.RuntimeGen != 2 {
		t.Errorf("after env change: rev=%d gen=%d env=%q, want 3/2/%q", got.Revision, got.RuntimeGen, got.Env, env)
	}
	if raw := rawColumn(t, db, "SELECT env FROM workspaces WHERE id = ?", p.ID); strings.Contains(raw, "sk-live") {
		t.Errorf("env at rest after update = %q, want sealed", raw)
	}

	cleared := *got
	cleared.Env = ""
	if _, err := workspaces.Update(ctx, p.ID, &cleared, 3, true); err != nil {
		t.Fatal(err)
	}
	if got, err = workspaces.Get(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got.Env != "" || got.RuntimeGen != 3 {
		t.Errorf("after clearing: env=%q gen=%d, want \"\"/3", got.Env, got.RuntimeGen)
	}

	stale := *got
	stale.Name = "loser"
	if _, err := workspaces.Update(ctx, p.ID, &stale, 3, false); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update: err=%v, want ErrRevisionConflict", err)
	}
	// The owner is identity, not editable content; the sandbox may move only
	// among sandboxes at the same address, so an unknown one is refused.
	moved := *got
	moved.OwnerID, moved.SandboxID = NewID(), NewID()
	if _, err := workspaces.Update(ctx, p.ID, &moved, 4, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update onto a missing sandbox: err=%v, want ErrNotFound", err)
	}
	moved.SandboxID = got.SandboxID
	if _, err := workspaces.Update(ctx, p.ID, &moved, 4, false); err != nil {
		t.Fatal(err)
	}
	if got, err = workspaces.Get(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got.OwnerID != LocalUserID || got.SandboxID != id("tg") {
		t.Errorf("owner/sandbox after update = %s/%s, want them unchanged", got.OwnerID, got.SandboxID)
	}
	if _, err := workspaces.Update(ctx, NewID(), &stale, 1, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing workspace: err=%v, want ErrNotFound", err)
	}
}

// A workspace may move between sandboxes that address the same machine — how it
// changes its image — and no further: its files live at that address and do
// not move with it.
func TestWorkspaceMovesOnlyWithinOneDestination(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	workspaces := NewWorkspaceStore(db)
	sandboxes := NewSandboxStore(db)

	mk := func(name, host, image string) *Sandbox {
		t.Helper()
		sb := &Sandbox{Name: name, Type: "docker", Config: []byte(`{"host":"` + host + `","image":"` + image + `"}`)}
		if err := sandboxes.Create(ctx, sb); err != nil {
			t.Fatal(err)
		}
		return sb
	}
	python := mk("python", "", "python:3.12")
	node := mk("node", "", "node:22")
	remote := mk("remote", "ssh://u@h", "node:22")

	p := &Workspace{ID: NewID(), OwnerID: LocalUserID, SandboxID: python.ID, Name: "p"}
	if err := workspaces.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	toNode := *p
	toNode.SandboxID = node.ID
	if _, err := workspaces.Update(ctx, p.ID, &toNode, 1, true); err != nil {
		t.Fatalf("moving to another image on the same daemon: %v", err)
	}
	got, err := workspaces.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != node.ID {
		t.Fatalf("sandbox = %s, want the node one", got.SandboxID)
	}

	toRemote := *got
	toRemote.SandboxID = remote.ID
	if _, err := workspaces.Update(ctx, p.ID, &toRemote, 2, true); !errors.Is(err, ErrSandboxMoveDestination) {
		t.Fatalf("moving to another daemon: err=%v, want ErrSandboxMoveDestination", err)
	}
}
