package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// createTarget persists a docker target under the given id, and createProject
// a project on it — a binding validates that its project exists before it is
// written, so tests must create what they bind.
func createSandbox(t *testing.T, r *Runner, id string) {
	t.Helper()
	sb := &store.Sandbox{ID: id, Name: id, Type: "docker", Config: json.RawMessage(`{"image":"i"}`)}
	if err := r.Deps.Sandboxes.Create(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox %s: %v", id, err)
	}
}

// createProject persists a project for LocalUserID on the given sandbox and
// returns its id.
func createProject(t *testing.T, r *Runner, id, sandboxID string) string {
	t.Helper()
	p := &store.Project{ID: id, OwnerID: store.LocalUserID, SandboxID: sandboxID, Name: id}
	if err := r.Deps.Projects.Create(context.Background(), p); err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	return p.ID
}

// startAndWait runs StartRun and blocks until the run terminates. The runs in
// these tests fail on config (no agent provider is wired) — that is fine:
// the project binding happens in startRunWithID BEFORE the launch, which is
// exactly the bind-at-start semantics under test.
func startAndWait(t *testing.T, r *Runner, sessionID, projectID string) string {
	t.Helper()
	done := make(chan struct{})
	runID, err := r.StartRun(sessionID, "", projectID, "hi", nil, func(*RunOutcome) { close(done) })
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("run %s did not finish", runID)
	}
	return runID
}

func countBoundEvents(t *testing.T, r *Runner, runID string) int {
	t.Helper()
	// The sink runs on the subscription's own goroutine and detach does not
	// join it, so the counter is atomic: the reads below can overlap a replay
	// still in flight.
	var n atomic.Int32
	seen := make(chan struct{})
	detach, ok := r.Hub().Subscribe(runID, 0, func(env *protocol.Envelope) {
		if env.Type == protocol.EventSessionProjectBound {
			n.Add(1)
		}
		select {
		case seen <- struct{}{}:
		default:
		}
	})
	if !ok {
		t.Fatalf("run %s not subscribable", runID)
	}
	defer detach()
	// The replay is synchronous-ish but rides the fanout goroutine: give it a
	// beat to flush. The run is over, so no more events will arrive after it.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-seen:
			continue
		case <-time.After(100 * time.Millisecond):
			return int(n.Load())
		case <-deadline:
			return int(n.Load())
		}
	}
}

// The first project-carrying run binds the session; later runs are overridden
// by the binding no matter what the client sends, and exactly one
// session.project_bound is published — by the winner.
func TestStartRunBindsSessionProject(t *testing.T) {
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-1")
	p1 := createProject(t, runner, "p-1", "tg-1")
	p2 := createProject(t, runner, "p-2", "tg-1")

	run1 := startAndWait(t, runner, sess.ID, p1)
	got, err := runner.Deps.Sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProjectID != p1 {
		t.Fatalf("bound to %q, want p-1", got.ProjectID)
	}
	if info, ok := runner.Hub().Info(run1); !ok || info.ProjectID != p1 {
		t.Fatalf("run1 info = %+v, want the bound project", info)
	}

	// A later run claiming a different project is overridden by the binding.
	run2 := startAndWait(t, runner, sess.ID, p2)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.ProjectID != p1 {
		t.Fatalf("binding rewritten to %q", got.ProjectID)
	}
	if info, ok := runner.Hub().Info(run2); !ok || info.ProjectID != p1 {
		t.Fatalf("run2 info = %+v, want the binding to override the request", info)
	}

	if n := countBoundEvents(t, runner, run1); n != 1 {
		t.Errorf("run1 published %d session.project_bound events, want 1", n)
	}
	if n := countBoundEvents(t, runner, run2); n != 0 {
		t.Errorf("run2 published %d session.project_bound events, want 0", n)
	}
}

// A run with no project binds nothing — the session stays bindable later.
func TestStartRunWithoutProjectLeavesSessionBindable(t *testing.T) {
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-late")

	startAndWait(t, runner, sess.ID, "")
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.ProjectID != "" {
		t.Fatalf("no-project run bound %q, want nothing", got.ProjectID)
	}

	late := createProject(t, runner, "p-late", "tg-late")
	startAndWait(t, runner, sess.ID, late)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.ProjectID != late {
		t.Fatalf("late bind = %q, want p-late", got.ProjectID)
	}
}

// A run refused at registration (session busy) must NOT have bound the
// session: the bind is written only after the hub accepts the run, so a 409
// leaves the session exactly as it was.
func TestRefusedRunDoesNotBind(t *testing.T) {
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-1")
	p := createProject(t, runner, "p-1", "tg-1")

	// Occupy the session's run slot directly, standing in for a live run.
	seg, _, err := runner.hub.register("run-live", sess.ID, "", "", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer runner.hub.unregister("run-live", seg)

	if _, err := runner.StartRun(sess.ID, "", p, "hi", nil, nil); err == nil {
		t.Fatal("StartRun succeeded on a busy session")
	}
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.ProjectID != "" {
		t.Fatalf("refused run bound the session to %q", got.ProjectID)
	}
}

// A binding is validated before anything is written: an unknown project, or
// one that is not the caller's, refuses the run and leaves the session unbound
// (and its slot free for the corrected retry).
func TestInvalidBindingRefusedUnbound(t *testing.T) {
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-bare")
	foreign := &store.Project{OwnerID: store.NewID(), SandboxID: "tg-bare", Name: "theirs"}
	if err := runner.Deps.Projects.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	for name, projectID := range map[string]string{
		"unknown project": store.NewID(),
		"foreign project": foreign.ID,
	} {
		_, err := runner.StartRun(sess.ID, "", projectID, "hi", nil, nil)
		if _, ok := errorsAsInvalidBinding(err); !ok {
			t.Errorf("%s: err = %v, want ErrInvalidBinding", name, err)
		}
	}
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.ProjectID != "" {
		t.Fatalf("invalid binding landed: %q", got.ProjectID)
	}
	// The slot is free: a corrected request binds normally.
	app := createProject(t, runner, "p-app", "tg-bare")
	startAndWait(t, runner, sess.ID, app)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.ProjectID != app {
		t.Fatalf("corrected bind = %q, want p-app", got.ProjectID)
	}
}

func errorsAsInvalidBinding(err error) (ErrInvalidBinding, bool) {
	var e ErrInvalidBinding
	if err == nil {
		return e, false
	}
	ok := errors.As(err, &e)
	return e, ok
}
