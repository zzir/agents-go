package store

import (
	"context"
	"errors"
	"testing"
)

// Create runs behind a lock on the sandbox row: a missing target refuses the
// insert instead of leaving a project row that points at nothing (decisions §5.28).
func TestProjectCreateRequiresSandbox(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	projects := NewProjectStore(db)

	p := &Project{ID: NewID(), OwnerID: LocalUserID, SandboxID: NewID(), Name: "p"}
	if err := projects.Create(ctx, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create on a missing sandbox: err=%v, want ErrNotFound", err)
	}

	createSandboxRow(t, db, id("sb-1"))
	p = &Project{ID: NewID(), OwnerID: LocalUserID, SandboxID: id("sb-1"), Name: "p"}
	if err := projects.Create(ctx, p); err != nil {
		t.Fatalf("create on an existing sandbox: %v", err)
	}
}

// List scopes to one owner — EveryOwner is the admin listing across all —
// and each row carries its bound-session count.
func TestProjectListScopeAndSessionCount(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	projects := NewProjectStore(db)

	createSandboxRow(t, db, id("sb"))
	mine := &Project{ID: NewID(), OwnerID: LocalUserID, SandboxID: id("sb"), Name: "mine"}
	foreign := &Project{ID: NewID(), OwnerID: NewID(), SandboxID: id("sb"), Name: "foreign"}
	for _, p := range []*Project{mine, foreign} {
		if err := projects.Create(ctx, p); err != nil {
			t.Fatalf("create project %s: %v", p.Name, err)
		}
	}
	s := &Session{ID: NewID(), OwnerID: LocalUserID, Name: "s", SandboxID: id("sb"), ProjectID: mine.ID}
	if err := NewSessionStore(db).Create(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}

	own, err := projects.List(ctx, LocalUserID)
	if err != nil {
		t.Fatalf("list own: %v", err)
	}
	if len(own) != 1 || own[0].ID != mine.ID {
		t.Fatalf("own listing = %+v, want just %s", own, mine.ID)
	}
	if own[0].SessionCount != 1 {
		t.Fatalf("session count = %d, want 1", own[0].SessionCount)
	}

	all, err := projects.List(ctx, EveryOwner)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("EveryOwner listing has %d rows, want 2", len(all))
	}
}
