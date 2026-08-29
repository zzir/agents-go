package sandboxes

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// testSpec is an in-memory build spec for manager tests: the manager reads
// the ids and the project's runtime generation, and the build override
// usually ignores the rest.
func testSpec(projectID string) Spec {
	return specGen(projectID, "", 1)
}

// specGen is testSpec with an explicit environment and runtime generation —
// what a project edit produces.
func specGen(projectID, env string, gen int64) Spec {
	return Spec{
		Sandbox: &store.Sandbox{ID: "sb", Name: "local", Type: "docker", Config: []byte(`{"image":"i"}`)},
		Project: &store.Project{ID: projectID, OwnerID: "owner-1", SandboxID: "sb", Name: projectID, Env: env, RuntimeGen: gen},
	}
}

// One live instance per project: sessions bound to different projects must
// not share a sandbox, while the same project keeps hitting the cache.
// RemoveProject tears down every generation of the id.
func TestSandboxManagerKeysByProject(t *testing.T) {
	m := NewManager()

	a1, r1, err := m.Acquire(testSpec("p1"))
	if err != nil {
		t.Fatalf("p1: %v", err)
	}
	defer r1()
	a2, r2, err := m.Acquire(testSpec("p2"))
	if err != nil {
		t.Fatalf("p2: %v", err)
	}
	defer r2()
	if a1 == a2 {
		t.Fatal("different projects share one sandbox instance")
	}
	again, r3, err := m.Acquire(testSpec("p1"))
	if err != nil {
		t.Fatalf("p1 again: %v", err)
	}
	defer r3()
	if again != a1 {
		t.Fatal("the same project was not served from the cache")
	}

	m.RemoveProject("p1")
	m.mu.Lock()
	_, left := m.instances[sandboxKey{projectID: "p1", gen: 1}]
	m.mu.Unlock()
	if left {
		t.Fatal("RemoveProject left the project cached")
	}
}

// An eviction with holders remaining defers the close to the LAST release —
// a run mid-flight or an open terminal keeps its instance alive — and nothing
// acquired after the eviction shares the doomed instance.
func TestSandboxManagerEvictionDefersToHolders(t *testing.T) {
	m := NewManager()

	inst1, rel1, err := m.acquire(testSpec("wd"))
	if err != nil {
		t.Fatal(err)
	}
	_, rel2, err := m.acquire(testSpec("wd"))
	if err != nil {
		t.Fatal(err)
	}

	m.EvictProject("wd")
	m.mu.Lock()
	doomed, refs := inst1.doomed, inst1.refs
	m.mu.Unlock()
	if !doomed || refs != 2 {
		t.Fatalf("after eviction with holders: doomed=%v refs=%d, want doomed with 2 refs", doomed, refs)
	}

	// A fresh acquire builds a NEW instance — the doomed one is out of the cache.
	inst2, rel3, err := m.acquire(testSpec("wd"))
	if err != nil {
		t.Fatal(err)
	}
	defer rel3()
	if inst2 == inst1 {
		t.Fatal("acquire after eviction returned the doomed instance")
	}

	// Releases are idempotent, and the last one closes the doomed instance.
	rel1()
	rel1()
	m.mu.Lock()
	refs = inst1.refs
	m.mu.Unlock()
	if refs != 1 {
		t.Fatalf("refs after double release = %d, want 1 — release must be once-guarded", refs)
	}
	rel2()
	m.mu.Lock()
	refs = inst1.refs
	m.mu.Unlock()
	if refs != 0 {
		t.Fatalf("refs after all releases = %d, want 0", refs)
	}
}

// An idle instance (no holders) closes immediately on eviction, and releasing
// an instance that is not doomed keeps it cached for the next acquire.
func TestSandboxManagerIdleEvictionAndReuse(t *testing.T) {
	m := NewManager()

	inst, rel, err := m.acquire(testSpec("wd"))
	if err != nil {
		t.Fatal(err)
	}
	rel()

	// Not doomed: the released instance stays cached and is reused.
	inst2, rel2, err := m.acquire(testSpec("wd"))
	if err != nil {
		t.Fatal(err)
	}
	if inst2 != inst {
		t.Fatal("released (but not evicted) instance was not reused")
	}
	rel2()

	m.EvictProject("wd")
	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("idle eviction left %d instances, want 0", left)
	}
}

// Concurrent acquires of one key share one build: every caller gets the same
// instance (the placeholder's ready gate synchronizes them), and the refcount
// equals the callers.
func TestSandboxManagerConcurrentAcquireSharesOneBuild(t *testing.T) {
	m := NewManager()

	const n = 8
	insts := make([]*sandboxInstance, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			inst, rel, err := m.acquire(testSpec("wd"))
			if err != nil {
				t.Error(err)
				return
			}
			insts[i], releases[i] = inst, rel
		})
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if insts[i] != insts[0] {
			t.Fatalf("acquire %d built a second instance for one key", i)
		}
	}
	m.mu.Lock()
	refs := insts[0].refs
	m.mu.Unlock()
	if refs != n {
		t.Fatalf("refs = %d, want %d", refs, n)
	}
	for _, rel := range releases {
		rel()
	}
}

// A failed build must not poison its key: the placeholder leaves the cache
// with the error, and the next acquire dials fresh. (The failing sandbox here
// has no image — refused before any daemon contact.)
func TestSandboxManagerFailedBuildRetries(t *testing.T) {
	m := NewManager()
	bad := testSpec("p")
	bad.Sandbox = &store.Sandbox{ID: "sb", Name: "empty", Type: "docker", Config: []byte(`{}`)}
	if _, _, err := m.acquire(bad); err == nil {
		t.Fatal("imageless docker build succeeded")
	}
	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("failed build left %d placeholders cached", left)
	}
	// Same key, now-valid template: the retry builds.
	inst, rel, err := m.acquire(testSpec("p"))
	if err != nil {
		t.Fatalf("retry after a failed build: %v", err)
	}
	defer rel()
	if inst.sb == nil {
		t.Fatal("retry returned a sandbox-less instance")
	}
}

// closeCountingSandbox records Close calls; everything else panics loudly via
// the embedded nil interface (the manager races never touch it).
type closeCountingSandbox struct {
	sandbox.Sandbox
	closes atomic.Int64
}

func (c *closeCountingSandbox) Close() error { c.closes.Add(1); return nil }

// gatedManager returns a manager whose builds block until the returned gate
// closes, handing each acquire its own countable sandbox.
func gatedManager(t *testing.T) (*Manager, chan struct{}, *closeCountingSandbox) {
	t.Helper()
	m := NewManager()
	gate := make(chan struct{})
	sb := &closeCountingSandbox{}
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) {
		<-gate
		return sb, nil
	}
	return m, gate, sb
}

// A content change while an old-generation build is dialing: the fence dooms
// the instance the moment its build lands — the run that started on it
// finishes and releases, then it closes. It never re-enters the cache for new
// runs. Every content change reaches the manager this way, whether it was the
// project's own environment, its template or its target (decisions §5.33).
func TestSandboxManagerRetireFencesInFlightBuilds(t *testing.T) {
	m, gate, sb := gatedManager(t)

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(testSpec("p"))
		if err != nil {
			t.Error(err)
			return
		}
		rel = r
	}()

	// The update lands mid-dial: generations below 2 are retired.
	m.RetireProject("p", 2)
	close(gate)
	<-done

	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("a retired-generation build re-entered the cache (%d instances)", left)
	}
	if sb.closes.Load() != 0 {
		t.Fatal("the instance closed while its holder was still using it")
	}
	rel()
	if sb.closes.Load() != 1 {
		t.Fatalf("closes = %d after the last release, want 1", sb.closes.Load())
	}
}

// CloseAll while a build is dialing: the placeholder is doomed, and the
// builder's freshly dialed resource is closed by the last release rather than
// leaked with no owner.
func TestSandboxManagerCloseAllDuringBuild(t *testing.T) {
	m, gate, sb := gatedManager(t)

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(testSpec("p"))
		if err != nil {
			t.Error(err)
			return
		}
		rel = r
	}()

	// Let the acquire install its placeholder before shutting down.
	for {
		m.mu.Lock()
		n := len(m.instances)
		m.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	m.CloseAll()
	close(gate)
	<-done

	rel()
	if sb.closes.Load() != 1 {
		t.Fatalf("closes = %d after shutdown + release, want 1 — the dialed resource leaked", sb.closes.Load())
	}
	// The latch refuses new acquires.
	if _, _, err := m.acquire(testSpec("p")); err == nil {
		t.Fatal("acquire succeeded on a closed manager")
	}
}

// A rename bumps the row revision but not the runtime generation, and the
// cache keys on the generation: the renamed project keeps sharing the live
// instance, and nothing retires it over a display-name edit.
func TestSandboxManagerRenameSharesInstance(t *testing.T) {
	m := NewManager()
	sb := &closeCountingSandbox{}
	builds := 0
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) {
		builds++
		return sb, nil
	}

	v1 := testSpec("p")
	v1.Project.Revision = 1
	_, rel1, err := m.Acquire(v1)
	if err != nil {
		t.Fatal(err)
	}
	renamed := testSpec("p")
	renamed.Project.Name, renamed.Project.Revision = "new", 2
	_, rel2, err := m.Acquire(renamed)
	if err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("builds = %d across a rename, want the one shared instance", builds)
	}
	rel1()
	rel2()
	if got := sb.closes.Load(); got != 0 {
		t.Fatalf("closes = %d, want 0 — a rename must not retire the instance", got)
	}

	// A CONTENT change moves the generation and does key a fresh instance.
	_, rel3, err := m.Acquire(specGen("p", "", 2))
	if err != nil {
		t.Fatal(err)
	}
	defer rel3()
	if builds != 2 {
		t.Fatalf("builds = %d after a content change, want 2", builds)
	}
}

// A delete landing between a caller's read and its acquire: the tombstone
// dooms the late build instead of letting it enter the cache as an instance
// of a deleted project that nothing would ever retire.
func TestSandboxManagerRemoveFencesLateAcquires(t *testing.T) {
	m, gate, sb := gatedManager(t)

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(testSpec("p"))
		if err != nil {
			t.Error(err)
			return
		}
		rel = r
	}()

	// The DELETE lands mid-dial: the id is gone for good.
	m.RemoveProject("p")
	close(gate)
	<-done

	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("a deleted project's build re-entered the cache (%d instances)", left)
	}
	if sb.closes.Load() != 0 {
		t.Fatal("the instance closed while its holder was still using it")
	}
	rel()
	if sb.closes.Load() != 1 {
		t.Fatalf("closes = %d after the last release, want 1", sb.closes.Load())
	}
}

// The idle-stop: the release that drops the last reference arms a timer that
// evicts and closes the instance; a new acquire before it fires disarms it.
func TestSandboxManagerIdleStop(t *testing.T) {
	m := NewManager()
	m.SetIdleTimeout(func() time.Duration { return 20 * time.Millisecond })
	closed := &closeCountingSandbox{}
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) { return closed, nil }

	inst, rel, err := m.acquire(testSpec("p"))
	if err != nil {
		t.Fatal(err)
	}
	// Re-acquire inside the idle window disarms the timer.
	rel()
	_, rel2, err := m.acquire(testSpec("p"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	m.mu.Lock()
	still := m.instances[inst.key] == inst
	m.mu.Unlock()
	if !still {
		t.Fatal("held instance was idle-stopped")
	}

	// The final release arms it; the fire evicts and closes.
	rel2()
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		gone := len(m.instances) == 0
		m.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle instance never evicted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if closed.closes.Load() != 1 {
		t.Fatalf("closes = %d, want 1", closed.closes.Load())
	}
}

// blockingCloseSandbox holds Close open until released — the idle-expiry
// window an acquire must wait out instead of adopting a stopping container.
type blockingCloseSandbox struct {
	sandbox.Sandbox
	closing chan struct{} // closed when Close was entered
	release chan struct{} // Close returns when this closes
	closes  atomic.Int64
}

func (b *blockingCloseSandbox) Close() error {
	close(b.closing)
	<-b.release
	b.closes.Add(1)
	return nil
}

// An acquire racing the idle expiry waits until the stop finished and then
// takes the key fresh — it must never join the stopping instance.
func TestSandboxManagerAcquireWaitsOutIdleExpiry(t *testing.T) {
	m := NewManager()
	m.SetIdleTimeout(func() time.Duration { return 5 * time.Millisecond })
	blocking := &blockingCloseSandbox{closing: make(chan struct{}), release: make(chan struct{})}
	fresh := &closeCountingSandbox{}
	first := true
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) {
		if first {
			first = false
			return blocking, nil
		}
		return fresh, nil
	}

	old, rel, err := m.acquire(testSpec("p"))
	if err != nil {
		t.Fatal(err)
	}
	rel() // arms the idle timer
	<-blocking.closing

	// The expiry is now mid-stop. A concurrent acquire must block on gone.
	got := make(chan *sandboxInstance, 1)
	go func() {
		inst, r, err := m.acquire(testSpec("p"))
		if err != nil {
			t.Error(err)
			got <- nil
			return
		}
		defer r()
		got <- inst
	}()
	select {
	case <-got:
		t.Fatal("acquire returned while the idle stop was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(blocking.release)
	select {
	case inst := <-got:
		if inst == nil || inst == old || inst.sb != fresh {
			t.Fatalf("acquire after the stop must build fresh, got %+v", inst)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire never returned after the stop finished")
	}
	if blocking.closes.Load() != 1 {
		t.Fatalf("old instance closes = %d, want 1", blocking.closes.Load())
	}
}

// A project's runtime generation is part of the cache key: a content change
// anywhere upstream must not keep being served from the instance built before
// it.
func TestSandboxManagerKeysByProjectGeneration(t *testing.T) {
	m := NewManager()
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) { return sandbox.NewLocal(), nil }

	a1, r1, err := m.Acquire(specGen("p", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	a2, r2, err := m.Acquire(specGen("p", `[{"key":"A","value":"1"}]`, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
	if a1 == a2 {
		t.Fatal("a new project generation was served the instance built from the old environment")
	}
	// The unchanged generation still hits the cache — a rename (revision
	// only) must not split it.
	again, r3, err := m.Acquire(specGen("p", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer r3()
	if again != a1 {
		t.Fatal("the same project generation was not served from the cache")
	}
}

// Retiring one project leaves its siblings' instances alone.
func TestRetireProjectSparesSiblings(t *testing.T) {
	m := NewManager()
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) { return sandbox.NewLocal(), nil }

	mine, r1, err := m.Acquire(testSpec("p1"))
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	sibling, r2, err := m.Acquire(testSpec("p2"))
	if err != nil {
		t.Fatal(err)
	}
	defer r2()

	m.RetireProject("p1", 2)

	stillCached, r3, err := m.Acquire(testSpec("p2"))
	if err != nil {
		t.Fatal(err)
	}
	defer r3()
	if stillCached != sibling {
		t.Error("retiring one project evicted another project's instance")
	}
	rebuilt, r4, err := m.Acquire(specGen("p1", "", 2))
	if err != nil {
		t.Fatal(err)
	}
	defer r4()
	if rebuilt == mine {
		t.Error("the retired project was served its old instance")
	}
}

// An environment that cannot be decoded refuses the build: a container
// started WITHOUT the variables it was configured with is worse than one
// that does not start.
func TestBuildSandboxRejectsUndecodableEnv(t *testing.T) {
	m := NewManager()
	_, _, err := m.Acquire(specGen("p", "{not json", 1))
	if err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("err = %v, want a project-environment failure", err)
	}
}

// DaemonOptions carries only how to reach the daemon; BuildOptions layers the
// image and the project's container and volume on top.
func TestDaemonAndBuildOptions(t *testing.T) {
	daemonOnly := &store.Sandbox{ID: "sb", Type: "docker", Config: []byte(`{"host":"ssh://u@h","ssh_use_agent":true}`)}
	opts, err := DaemonOptions(daemonOnly)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "ssh://u@h" || !opts.SSH.UseAgent {
		t.Errorf("daemon options = %+v", opts)
	}
	if opts.Image != "" || opts.ContainerName != "" || opts.VolumeName != "" || opts.Persistent {
		t.Errorf("daemon options carry image or project settings: %+v", opts)
	}
	if _, err := DaemonOptions(&store.Sandbox{ID: "x", Type: "podman"}); err == nil {
		t.Error("a non-docker sandbox type was accepted")
	}

	spec := Spec{
		Sandbox: &store.Sandbox{ID: "sb", Type: "docker", Config: []byte(`{"host":"ssh://u@h","ssh_use_agent":true,"image":"img","memory_mb":256,"cpus":2,"network":"agents-net"}`)},
		Project: &store.Project{ID: "proj", Name: "proj"},
	}
	full, err := BuildOptions(spec)
	if err != nil {
		t.Fatal(err)
	}
	if full.Image != "img" || full.Limits.MemoryBytes != 256<<20 || full.Limits.CPUs != 2 || full.Network != "agents-net" {
		t.Errorf("build options = %+v", full)
	}
	if full.User != DefaultContainerUser {
		t.Errorf("user = %q, want %q — a sandbox naming none runs as root", full.User, DefaultContainerUser)
	}
	if !full.Persistent || full.ContainerName != ContainerName("proj") || full.VolumeName != ProjectVolumeName("proj") {
		t.Errorf("build options miss the project's container or volume: %+v", full)
	}
	if full.WorkDir != "" {
		t.Errorf("build options carry a host bind mount: %q", full.WorkDir)
	}
	spec.Sandbox = &store.Sandbox{ID: "sb", Type: "docker", Config: []byte(`{}`)}
	if _, err := BuildOptions(spec); err == nil {
		t.Error("a sandbox without an image was accepted")
	}
}

// lifecycleSandbox is a countable sandbox that also implements Lifecycle.
type lifecycleSandbox struct {
	closeCountingSandbox
	state   sandbox.State
	starts  int
	stops   int
	stopErr error
}

func (l *lifecycleSandbox) Start(context.Context) error {
	l.starts++
	l.state = sandbox.StateRunning
	return nil
}

func (l *lifecycleSandbox) Stop(context.Context) error {
	if l.stopErr != nil {
		return l.stopErr
	}
	l.stops++
	l.state = sandbox.StateStopped
	return nil
}

func (l *lifecycleSandbox) Status(context.Context) (sandbox.State, error) { return l.state, nil }

func lifecycleManager(t *testing.T) (*Manager, *lifecycleSandbox) {
	t.Helper()
	m := NewManager()
	sb := &lifecycleSandbox{}
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) { return sb, nil }
	return m, sb
}

// EnsureRunning starts the sandbox and Status reports it; the reference the
// call took is released, so nothing is left holding the instance.
func TestManagerEnsureRunningAndStatus(t *testing.T) {
	m, sb := lifecycleManager(t)
	if got, err := m.Status(t.Context(), testSpec("p")); err != nil || got != sandbox.StateAbsent {
		t.Fatalf("Status before start = %v, %v; want absent", got, err)
	}
	if err := m.EnsureRunning(t.Context(), testSpec("p")); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got, err := m.Status(t.Context(), testSpec("p")); err != nil || got != sandbox.StateRunning {
		t.Fatalf("Status after start = %v, %v; want running", got, err)
	}
	if sb.starts != 1 {
		t.Errorf("starts = %d, want 1", sb.starts)
	}
	if n := m.holders("p"); n != 0 {
		t.Errorf("holders after the calls returned = %d, want 0", n)
	}
}

// An idle sandbox stops NOW, and the instance leaves the cache so the next
// acquire builds against the current configuration.
func TestManagerStopIdle(t *testing.T) {
	m, sb := lifecycleManager(t)
	if err := m.EnsureRunning(t.Context(), testSpec("p")); err != nil {
		t.Fatal(err)
	}
	stopped, err := m.Stop(t.Context(), testSpec("p"))
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v; want an immediate stop", stopped, err)
	}
	if sb.stops != 1 {
		t.Errorf("stops = %d, want 1", sb.stops)
	}
	m.mu.Lock()
	cached := len(m.instances)
	m.mu.Unlock()
	if cached != 0 {
		t.Errorf("instances after Stop = %d, want 0", cached)
	}
}

// A Stop while a run holds the sandbox does NOT tear it off its container: it
// reports the stop as deferred, dooms the instance so nothing new joins, and
// the holder's release is what PAUSES it (Lifecycle.Stop) — not merely closes
// the connection, which for e2b would leave the billed sandbox running.
func TestManagerStopDefersToHolders(t *testing.T) {
	m, sb := lifecycleManager(t)
	held, release, err := m.acquire(testSpec("p"))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := m.Stop(t.Context(), testSpec("p"))
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped {
		t.Fatal("Stop reported an immediate stop while a holder was using it")
	}
	if sb.stops != 0 {
		t.Errorf("stops = %d, want 0 — the holder was still working", sb.stops)
	}
	m.mu.Lock()
	doomed := held.doomed
	m.mu.Unlock()
	if !doomed {
		t.Error("the instance was not doomed, so a later acquire could still join it")
	}
	release()
	if sb.stops != 1 {
		t.Errorf("stops after the last release = %d, want 1 — the deferred stop must pause the compute", sb.stops)
	}
	if sb.closes.Load() != 1 {
		t.Errorf("closes after the last release = %d, want 1", sb.closes.Load())
	}
}

// A backend with no Lifecycle says so rather than pretending.
func TestManagerLifecycleUnsupported(t *testing.T) {
	m := NewManager()
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) { return &closeCountingSandbox{}, nil }
	if err := m.EnsureRunning(t.Context(), testSpec("p")); !errors.Is(err, sandbox.ErrLifecycleUnsupported) {
		t.Errorf("EnsureRunning = %v, want ErrLifecycleUnsupported", err)
	}
	if _, err := m.Stop(t.Context(), testSpec("p")); !errors.Is(err, sandbox.ErrLifecycleUnsupported) {
		t.Errorf("Stop = %v, want ErrLifecycleUnsupported", err)
	}
	if _, err := m.Status(t.Context(), testSpec("p")); !errors.Is(err, sandbox.ErrLifecycleUnsupported) {
		t.Errorf("Status = %v, want ErrLifecycleUnsupported", err)
	}
}

// An unknown target type fails loudly at build time — a stored row this build
// does not carry must not read as a working sandbox.
func TestBackendForUnknownType(t *testing.T) {
	m := NewManager()
	spec := testSpec("p")
	spec.Sandbox.Type = "quantum"
	if _, _, err := m.Acquire(spec); err == nil || !strings.Contains(err.Error(), "quantum") {
		t.Fatalf("Acquire on an unknown type = %v, want a refusal naming it", err)
	}
}

// blockingStopSandbox is a Lifecycle sandbox whose Stop holds until released —
// the window a user Stop is mid-stop and a racing acquire must wait out.
type blockingStopSandbox struct {
	closeCountingSandbox
	stopping chan struct{} // closed when Stop was entered
	release  chan struct{} // Stop returns when this closes
}

func (b *blockingStopSandbox) Start(context.Context) error { return nil }

func (b *blockingStopSandbox) Status(context.Context) (sandbox.State, error) {
	return sandbox.StateRunning, nil
}

func (b *blockingStopSandbox) Stop(context.Context) error {
	close(b.stopping)
	<-b.release
	return nil
}

// A user Stop fences the instance the way the idle expiry does: an acquire
// racing the stop waits until it completes and then builds fresh — it must
// never build against the container mid-stop.
func TestManagerAcquireWaitsOutUserStop(t *testing.T) {
	m := NewManager()
	blocking := &blockingStopSandbox{stopping: make(chan struct{}), release: make(chan struct{})}
	fresh := &closeCountingSandbox{}
	first := true
	m.buildOverride = func(Spec) (sandbox.Sandbox, error) {
		if first {
			first = false
			return blocking, nil
		}
		return fresh, nil
	}

	stopped := make(chan bool, 1)
	go func() {
		ok, err := m.Stop(context.Background(), testSpec("p"))
		if err != nil {
			t.Error(err)
		}
		stopped <- ok
	}()
	<-blocking.stopping

	// The Stop is mid-flight. A concurrent acquire must block on gone.
	got := make(chan *sandboxInstance, 1)
	go func() {
		inst, r, err := m.acquire(testSpec("p"))
		if err != nil {
			t.Error(err)
			got <- nil
			return
		}
		defer r()
		got <- inst
	}()
	select {
	case <-got:
		t.Fatal("acquire returned while the user Stop was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(blocking.release)
	if ok := <-stopped; !ok {
		t.Fatal("Stop with no other holder must report an immediate stop")
	}
	select {
	case inst := <-got:
		if inst == nil || inst.sb != fresh {
			t.Fatalf("acquire after the stop must build fresh, got %+v", inst)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire never returned after the stop finished")
	}
}

// A deferred Stop that new work overtook is superseded: the last holder's
// release must not pause the container out from under the new acquire.
func TestManagerDeferredStopSupersededByNewAcquire(t *testing.T) {
	m, sb := lifecycleManager(t)
	_, release, err := m.acquire(testSpec("p"))
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := m.Stop(t.Context(), testSpec("p"))
	if err != nil || stopped {
		t.Fatalf("Stop = %v, %v; want a deferred stop", stopped, err)
	}
	// New work arrives before the holder finishes: the stale stop must yield.
	_, r2, err := m.acquire(testSpec("p"))
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
	release()
	if sb.stops != 0 {
		t.Fatalf("stops = %d, want 0 — the superseded stop paused the new acquire's container", sb.stops)
	}
	if sb.closes.Load() != 1 {
		t.Errorf("closes = %d, want 1 — the doomed instance's connection still closes", sb.closes.Load())
	}
}
