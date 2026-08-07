package tasks

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// fakeLauncher records launches and can be made to fail.
type fakeLauncher struct {
	mu       sync.Mutex
	launched []LaunchRequest
	err      error
	// beforeLaunch runs inside Launch, before the request is recorded. It is
	// how a test stages something arriving in the window between a task
	// claiming a run and the host knowing that run exists.
	beforeLaunch func(LaunchRequest)
}

func (l *fakeLauncher) Launch(_ context.Context, req LaunchRequest) error {
	if hook := l.hook(); hook != nil {
		hook(req)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.launched = append(l.launched, req)
	return nil
}

// hook reads beforeLaunch under the lock, so a test may set it from another
// goroutine, and calls it OUTSIDE — the hook re-enters the Manager, which
// launches again.
func (l *fakeLauncher) hook() func(LaunchRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.beforeLaunch
}

func (l *fakeLauncher) all() []LaunchRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LaunchRequest(nil), l.launched...)
}

func (l *fakeLauncher) wakes() []LaunchRequest {
	var out []LaunchRequest
	for _, r := range l.all() {
		if r.Wake {
			out = append(out, r)
		}
	}
	return out
}

// countingRepo is a SessionRepo over in-memory storage that records deletes, so
// a test can assert cleanup happened.
type countingRepo struct {
	mu       sync.Mutex
	repo     session.Repo
	created  []string
	deleted  []string
	failNext bool
}

func newCountingRepo() *countingRepo {
	return &countingRepo{repo: session.NewInMemoryRepo()}
}

func (r *countingRepo) Create(ctx context.Context, opts session.CreateOptions) (*session.Session, error) {
	r.mu.Lock()
	if r.failNext {
		r.failNext = false
		r.mu.Unlock()
		return nil, context.Canceled
	}
	r.created = append(r.created, opts.ID)
	r.mu.Unlock()
	return r.repo.Create(ctx, opts)
}

func (r *countingRepo) Open(ctx context.Context, id string) (*session.Session, error) {
	return r.repo.Open(ctx, id)
}

func (r *countingRepo) List(ctx context.Context, opts session.ListOptions) ([]session.Metadata, error) {
	return r.repo.List(ctx, opts)
}

func (r *countingRepo) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	r.deleted = append(r.deleted, id)
	r.mu.Unlock()
	return r.repo.Delete(ctx, id)
}

func (r *countingRepo) deletes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

// harness wires a Manager with controllable pieces.
type harness struct {
	m        *Manager
	store    *InMemoryStore
	launcher *fakeLauncher
	repo     *countingRepo
	// canWake and guardErr drive the WakeGuard.
	canWake bool
	stopped []string
	mu      sync.Mutex
}

func newHarness(t *testing.T, tune ...func(*Config)) *harness {
	t.Helper()
	h := &harness{
		store:    NewInMemoryStore(),
		launcher: &fakeLauncher{},
		repo:     newCountingRepo(),
		canWake:  true,
	}
	cfg := Config{
		Store:    h.store,
		Sessions: h.repo,
		Launcher: h.launcher,
		Resolver: AgentResolverFunc(func(_ context.Context, _, name string) (Spec, error) {
			if name == "" {
				name = "default"
			}
			return Spec{DisplayName: name, Inherit: json.RawMessage(`{"agent":"` + name + `"}`)}, nil
		}),
		Guard: WakeGuardFunc(func(context.Context, string) bool { return h.canWake }),
		// A host that knows every run it was asked about: the tests that care
		// about the launch window override this.
		Stopper: StopperFunc(func(_ context.Context, runID string, graceful bool) (StopOutcome, error) {
			h.mu.Lock()
			h.stopped = append(h.stopped, runID)
			h.mu.Unlock()
			if graceful {
				return StopAfterTurn, nil
			}
			return StopCancelled, nil
		}),
	}
	for _, f := range tune {
		f(&cfg)
	}
	h.m = New(cfg)
	return h
}

// spawn starts a task for the harness's standard parent session.
func (h *harness) spawn(t *testing.T) *Info {
	t.Helper()
	info, err := h.m.Spawn(context.Background(), SpawnRequest{
		ParentSessionID: "parent", AgentName: "worker", Input: "do it", Label: "job",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return info
}

func (h *harness) get(t *testing.T, id string) *Task {
	t.Helper()
	task, err := h.store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	return task
}

// childOf returns the child session id of a task.
func (h *harness) childOf(t *testing.T, id string) string {
	t.Helper()
	return h.get(t, id).ChildSessionID
}
