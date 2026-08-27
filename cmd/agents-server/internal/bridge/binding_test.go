package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The binding-time project rules: a run naming no project resolves to none
// (no project, no sandbox tools — decisions §5.33); a named one must exist and
// be the session owner's.
func TestPlanProjectBinding(t *testing.T) {
	runner, db := newBareRunner(t)
	ctx := context.Background()
	targets := store.NewSandboxStore(db)
	tg := &store.Sandbox{ID: store.NewID(), Name: "tg", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := targets.Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	mine := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "mine"}
	if err := runner.Deps.Projects.Create(ctx, mine); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{ID: store.NewID(), OwnerID: store.LocalUserID, Name: "s"}

	// Unnamed: no project, and nothing to bind.
	plan, err := runner.planProjectBinding(ctx, sess, "")
	if err != nil {
		t.Fatalf("unnamed: %v", err)
	}
	if plan.projectID != "" || plan.needBind {
		t.Fatalf("unnamed plan = %+v, want an empty plan", plan)
	}

	// Named: the owner's project resolves and a bind is planned.
	plan, err = runner.planProjectBinding(ctx, sess, mine.ID)
	if err != nil || plan.projectID != mine.ID || !plan.needBind {
		t.Fatalf("named plan = %+v, %v", plan, err)
	}

	// A bound session overrides whatever the request says.
	bound := &store.Session{ID: store.NewID(), OwnerID: store.LocalUserID, Name: "b", ProjectID: mine.ID}
	plan, err = runner.planProjectBinding(ctx, bound, store.NewID())
	if err != nil || plan.projectID != mine.ID || plan.needBind {
		t.Fatalf("bound plan = %+v, %v; want the standing binding and no re-bind", plan, err)
	}

	var invalid ErrInvalidBinding
	if _, err := runner.planProjectBinding(ctx, sess, store.NewID()); !errors.As(err, &invalid) {
		t.Fatalf("unknown project: %v, want ErrInvalidBinding", err)
	}
	// A foreign owner's project reads as absent.
	foreign := &store.Project{OwnerID: store.NewID(), SandboxID: tg.ID, Name: "theirs"}
	if err := runner.Deps.Projects.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.planProjectBinding(ctx, sess, foreign.ID); !errors.As(err, &invalid) {
		t.Fatalf("foreign project: %v, want ErrInvalidBinding", err)
	}
}
