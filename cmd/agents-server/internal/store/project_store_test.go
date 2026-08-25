package store

import (
	"context"
	"errors"
	"testing"
)

// Create runs behind a lock on the sandbox row: a missing target refuses the
// insert instead of leaving a project row that points at nothing (spec §5.28).
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
