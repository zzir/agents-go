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
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// createTarget persists a docker target under the given id, and createWorkspace
// a workspace on it — a binding validates that its workspace exists before it is
// written, so tests must create what they bind.
func createSandbox(t *testing.T, r *Runner, id string) {
	t.Helper()
	sb := &store.Sandbox{ID: id, Name: id, Type: "docker", Config: json.RawMessage(`{"image":"i"}`)}
	if err := r.Deps.Sandboxes.Create(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox %s: %v", id, err)
	}
}

// createWorkspace persists a workspace for LocalUserID on the given sandbox and
// returns its id.
func createWorkspace(t *testing.T, r *Runner, id, sandboxID string) string {
	t.Helper()
	p := &store.Workspace{ID: id, OwnerID: store.LocalUserID, SandboxID: sandboxID, Name: id}
	if err := r.Deps.Workspaces.Create(context.Background(), p); err != nil {
		t.Fatalf("create workspace %s: %v", id, err)
	}
	return p.ID
}

// startAndWait runs StartRun and blocks until the run terminates. The runs in
// these tests fail on config (no agent provider is wired) — that is fine:
// the workspace binding happens in startRunWithID BEFORE the launch, which is
// exactly the bind-at-start semantics under test.
func startAndWait(t *testing.T, r *Runner, sessionID, workspaceID string) string {
	t.Helper()
	done := make(chan struct{})
	runID, err := r.StartRun(sessionID, "", workspaceID, TextInput("hi"), nil, func(*RunOutcome) { close(done) })
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
		if env.Type == protocol.EventSessionWorkspaceBound {
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

// The first workspace-carrying run binds the session; later runs are overridden
// by the binding no matter what the client sends, and exactly one
// session.workspace_bound is published — by the winner.
func TestStartRunBindsSessionWorkspace(t *testing.T) {
	testdb.SkipOnPostgres(t)
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-1")
	p1 := createWorkspace(t, runner, "p-1", "tg-1")
	p2 := createWorkspace(t, runner, "p-2", "tg-1")

	run1 := startAndWait(t, runner, sess.ID, p1)
	got, err := runner.Deps.Sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorkspaceID != p1 {
		t.Fatalf("bound to %q, want p-1", got.WorkspaceID)
	}
	if info, ok := runner.Hub().Info(run1); !ok || info.WorkspaceID != p1 {
		t.Fatalf("run1 info = %+v, want the bound workspace", info)
	}

	// A later run claiming a different workspace is overridden by the binding.
	run2 := startAndWait(t, runner, sess.ID, p2)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.WorkspaceID != p1 {
		t.Fatalf("binding rewritten to %q", got.WorkspaceID)
	}
	if info, ok := runner.Hub().Info(run2); !ok || info.WorkspaceID != p1 {
		t.Fatalf("run2 info = %+v, want the binding to override the request", info)
	}

	if n := countBoundEvents(t, runner, run1); n != 1 {
		t.Errorf("run1 published %d session.workspace_bound events, want 1", n)
	}
	if n := countBoundEvents(t, runner, run2); n != 0 {
		t.Errorf("run2 published %d session.workspace_bound events, want 0", n)
	}
}

// A run with no workspace binds nothing — the session stays bindable later.
func TestStartRunWithoutWorkspaceLeavesSessionBindable(t *testing.T) {
	testdb.SkipOnPostgres(t)
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-late")

	startAndWait(t, runner, sess.ID, "")
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.WorkspaceID != "" {
		t.Fatalf("no-workspace run bound %q, want nothing", got.WorkspaceID)
	}

	late := createWorkspace(t, runner, "p-late", "tg-late")
	startAndWait(t, runner, sess.ID, late)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.WorkspaceID != late {
		t.Fatalf("late bind = %q, want p-late", got.WorkspaceID)
	}
}

// A run refused at registration (session busy) must NOT have bound the
// session: the bind is written only after the hub accepts the run, so a 409
// leaves the session exactly as it was.
func TestRefusedRunDoesNotBind(t *testing.T) {
	testdb.SkipOnPostgres(t)
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-1")
	p := createWorkspace(t, runner, "p-1", "tg-1")

	// Occupy the session's run slot directly, standing in for a live run.
	seg, _, err := runner.hub.register("run-live", sess.ID, "", "", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer runner.hub.unregister("run-live", seg)

	if _, err := runner.StartRun(sess.ID, "", p, TextInput("hi"), nil, nil); err == nil {
		t.Fatal("StartRun succeeded on a busy session")
	}
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.WorkspaceID != "" {
		t.Fatalf("refused run bound the session to %q", got.WorkspaceID)
	}
}

// A binding is validated before anything is written: an unknown workspace, or
// one that is not the caller's, refuses the run and leaves the session unbound
// (and its slot free for the corrected retry).
func TestInvalidBindingRefusedUnbound(t *testing.T) {
	testdb.SkipOnPostgres(t)
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandbox(t, runner, "tg-bare")
	foreign := &store.Workspace{OwnerID: store.NewID(), SandboxID: "tg-bare", Name: "theirs"}
	if err := runner.Deps.Workspaces.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	for name, workspaceID := range map[string]string{
		"unknown workspace": store.NewID(),
		"foreign workspace": foreign.ID,
	} {
		_, err := runner.StartRun(sess.ID, "", workspaceID, TextInput("hi"), nil, nil)
		if _, ok := errorsAsInvalidBinding(err); !ok {
			t.Errorf("%s: err = %v, want ErrInvalidBinding", name, err)
		}
	}
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.WorkspaceID != "" {
		t.Fatalf("invalid binding landed: %q", got.WorkspaceID)
	}
	// The slot is free: a corrected request binds normally.
	app := createWorkspace(t, runner, "p-app", "tg-bare")
	startAndWait(t, runner, sess.ID, app)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.WorkspaceID != app {
		t.Fatalf("corrected bind = %q, want p-app", got.WorkspaceID)
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
