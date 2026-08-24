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

// createSandboxConfig persists a docker config under the given id — a
// binding validates that its sandbox exists before it is written, so tests
// must create what they bind.
func createSandboxConfig(t *testing.T, r *Runner, id string) {
	t.Helper()
	cfg := &store.SandboxConfig{ID: id, Name: id, Type: "docker",
		Config: json.RawMessage(`{"image":"i"}`)}
	if err := r.Deps.SandboxConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatalf("create sandbox config %s: %v", id, err)
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
// the sandbox binding happens in startRunWithID BEFORE the launch, which is
// exactly the bind-at-start semantics under test.
func startAndWait(t *testing.T, r *Runner, sessionID, sandboxID, workDir string) string {
	t.Helper()
	done := make(chan struct{})
	runID, err := r.StartRun(sessionID, "", sandboxID, workDir, "hi", nil, func(*RunOutcome) { close(done) })
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
		if env.Type == protocol.EventSessionSandboxBound {
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

// The first sandbox-carrying run binds the session; later runs are overridden
// by the binding no matter what the client sends, and exactly one
// session.sandbox_bound is published — by the winner.
func TestStartRunBindsSessionSandbox(t *testing.T) {
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxConfig(t, runner, "sb-1")
	createSandboxConfig(t, runner, "sb-2")
	p1 := createProject(t, runner, "p-1", "sb-1")
	p2 := createProject(t, runner, "p-2", "sb-2")

	run1 := startAndWait(t, runner, sess.ID, "sb-1", p1)
	got, err := runner.Deps.Sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SandboxID != "sb-1" || got.ProjectID != p1 {
		t.Fatalf("bound to (%q,%q), want (sb-1,p-1)", got.SandboxID, got.ProjectID)
	}
	if info, ok := runner.Hub().Info(run1); !ok || info.SandboxID != "sb-1" || info.ProjectID != p1 {
		t.Fatalf("run1 info = %+v, want the bound pair", info)
	}

	// A later run claiming a different sandbox is overridden by the binding.
	run2 := startAndWait(t, runner, sess.ID, "sb-2", p2)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.SandboxID != "sb-1" || got.ProjectID != p1 {
		t.Fatalf("binding rewritten to (%q,%q)", got.SandboxID, got.ProjectID)
	}
	if info, ok := runner.Hub().Info(run2); !ok || info.SandboxID != "sb-1" || info.ProjectID != p1 {
		t.Fatalf("run2 info = %+v, want the binding to override the request", info)
	}

	if n := countBoundEvents(t, runner, run1); n != 1 {
		t.Errorf("run1 published %d session.sandbox_bound events, want 1", n)
	}
	if n := countBoundEvents(t, runner, run2); n != 0 {
		t.Errorf("run2 published %d session.sandbox_bound events, want 0", n)
	}
}

// A run with no sandbox binds nothing — the session stays bindable later.
func TestStartRunWithoutSandboxLeavesSessionBindable(t *testing.T) {
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxConfig(t, runner, "sb-late")

	startAndWait(t, runner, sess.ID, "", "/ignored")
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.SandboxID != "" || got.ProjectID != "" {
		t.Fatalf("no-sandbox run bound (%q,%q), want nothing", got.SandboxID, got.ProjectID)
	}

	// An unnamed project resolves to the auto-created default.
	startAndWait(t, runner, sess.ID, "sb-late", "")
	got, _ := runner.Deps.Sessions.Get(ctx, sess.ID)
	if got.SandboxID != "sb-late" || got.ProjectID == "" {
		t.Fatalf("late bind = (%q,%q), want (sb-late, the default project)", got.SandboxID, got.ProjectID)
	}
	if p, err := runner.Deps.Projects.Get(ctx, got.ProjectID); err != nil || p.Name != store.DefaultProjectName {
		t.Fatalf("default project = %+v, %v", p, err)
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
	createSandboxConfig(t, runner, "sb-1")

	// Occupy the session's run slot directly, standing in for a live run.
	seg, _, err := runner.hub.register("run-live", sess.ID, "", "", "", "", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer runner.hub.unregister("run-live", seg)

	if _, err := runner.StartRun(sess.ID, "", "sb-1", "", "hi", nil, nil); err == nil {
		t.Fatal("StartRun succeeded on a busy session")
	}
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.SandboxID != "" {
		t.Fatalf("refused run bound the session to %q", got.SandboxID)
	}
}

// A binding is validated before anything is written: an unknown sandbox id,
// or a project that is not the caller's on that sandbox, refuses the run and
// leaves the session unbound (and its slot free for the corrected retry).
func TestInvalidBindingRefusedUnbound(t *testing.T) {
	runner, _ := newBareRunner(t)
	ctx := context.Background()

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxConfig(t, runner, "sb-bare")
	createSandboxConfig(t, runner, "sb-other")
	elsewhere := createProject(t, runner, "p-elsewhere", "sb-other")

	for name, req := range map[string]struct{ sandboxID, projectID string }{
		"unknown sandbox":       {"sb-ghost", ""},
		"unknown project":       {"sb-bare", store.NewID()},
		"cross-sandbox project": {"sb-bare", elsewhere},
	} {
		_, err := runner.StartRun(sess.ID, "", req.sandboxID, req.projectID, "hi", nil, nil)
		if _, ok := errorsAsInvalidBinding(err); !ok {
			t.Errorf("%s: err = %v, want ErrInvalidBinding", name, err)
		}
	}
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.SandboxID != "" {
		t.Fatalf("invalid binding landed: %q", got.SandboxID)
	}
	// The slot is free: a corrected request binds normally.
	app := createProject(t, runner, "p-app", "sb-bare")
	startAndWait(t, runner, sess.ID, "sb-bare", app)
	if got, _ := runner.Deps.Sessions.Get(ctx, sess.ID); got.SandboxID != "sb-bare" || got.ProjectID != app {
		t.Fatalf("corrected bind = (%q,%q), want (sb-bare,p-app)", got.SandboxID, got.ProjectID)
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
