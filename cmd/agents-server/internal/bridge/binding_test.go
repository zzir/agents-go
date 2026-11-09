package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The binding-time workspace rules: a run naming no workspace resolves to none
// (no workspace, no sandbox tools — decisions §5.33); a named one must exist and
// be the session owner's.
func TestPlanWorkspaceBinding(t *testing.T) {
	runner, db := newBareRunner(t)
	ctx := context.Background()
	targets := store.NewSandboxStore(db)
	tg := &store.Sandbox{ID: store.NewID(), Name: "tg", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := targets.Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	mine := &store.Workspace{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "mine"}
	if err := runner.Deps.Workspaces.Create(ctx, mine); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{ID: store.NewID(), OwnerID: store.LocalUserID, Name: "s"}

	// Unnamed: no workspace, and nothing to bind.
	plan, err := runner.planWorkspaceBinding(ctx, sess, "")
	if err != nil {
		t.Fatalf("unnamed: %v", err)
	}
	if plan.workspaceID != "" || plan.needBind {
		t.Fatalf("unnamed plan = %+v, want an empty plan", plan)
	}

	// Named: the owner's workspace resolves and a bind is planned.
	plan, err = runner.planWorkspaceBinding(ctx, sess, mine.ID)
	if err != nil || plan.workspaceID != mine.ID || !plan.needBind {
		t.Fatalf("named plan = %+v, %v", plan, err)
	}

	// A bound session overrides whatever the request says.
	bound := &store.Session{ID: store.NewID(), OwnerID: store.LocalUserID, Name: "b", WorkspaceID: mine.ID}
	plan, err = runner.planWorkspaceBinding(ctx, bound, store.NewID())
	if err != nil || plan.workspaceID != mine.ID || plan.needBind {
		t.Fatalf("bound plan = %+v, %v; want the standing binding and no re-bind", plan, err)
	}

	var invalid ErrInvalidBinding
	if _, err := runner.planWorkspaceBinding(ctx, sess, store.NewID()); !errors.As(err, &invalid) {
		t.Fatalf("unknown workspace: %v, want ErrInvalidBinding", err)
	}
	// A foreign owner's workspace reads as absent.
	foreign := &store.Workspace{OwnerID: store.NewID(), SandboxID: tg.ID, Name: "theirs"}
	if err := runner.Deps.Workspaces.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.planWorkspaceBinding(ctx, sess, foreign.ID); !errors.As(err, &invalid) {
		t.Fatalf("foreign workspace: %v, want ErrInvalidBinding", err)
	}
}
