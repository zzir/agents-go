package sandboxes

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// testProject is an in-memory project row for manager tests: the manager
// only reads its ids.
func testProject(id string) *store.Project {
	return &store.Project{ID: id, OwnerID: "owner-1", SandboxID: "loc", Name: id}
}

// One live instance per (config id, project): sessions bound to different
// projects on the same config must not share a sandbox, while the same pair
// keeps hitting the cache. Remove tears down every variant of the id.
func TestSandboxManagerKeysByProject(t *testing.T) {
	m := NewManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "docker", Config: []byte(`{"image":"i"}`)}

	a1, r1, err := m.Acquire(cfg, testProject("p1"))
	if err != nil {
		t.Fatalf("p1: %v", err)
	}
	defer r1()
	a2, r2, err := m.Acquire(cfg, testProject("p2"))
	if err != nil {
		t.Fatalf("p2: %v", err)
	}
	defer r2()
	if a1 == a2 {
		t.Fatal("different projects share one sandbox instance")
	}
	again, r3, err := m.Acquire(cfg, testProject("p1"))
	if err != nil {
		t.Fatalf("p1 again: %v", err)
	}
	defer r3()
	if again != a1 {
		t.Fatal("same (id, project) pair not served from the cache")
	}

	m.Remove("loc")
	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("Remove left %d variants cached, want 0", left)
	}
}

// An eviction with holders remaining defers the close to the LAST release —
// a run mid-flight or an open terminal keeps its instance alive — and nothing
// acquired after the eviction shares the doomed instance.
func TestSandboxManagerEvictionDefersToHolders(t *testing.T) {
	m := NewManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "docker", Config: []byte(`{"image":"i"}`)}

	inst1, rel1, err := m.acquire(cfg, testProject("wd"))
	if err != nil {
		t.Fatal(err)
	}
	_, rel2, err := m.acquire(cfg, testProject("wd"))
	if err != nil {
		t.Fatal(err)
	}

	m.RemoveInstance(cfg, "wd")
	m.mu.Lock()
	doomed, refs := inst1.doomed, inst1.refs
	m.mu.Unlock()
	if !doomed || refs != 2 {
		t.Fatalf("after eviction with holders: doomed=%v refs=%d, want doomed with 2 refs", doomed, refs)
	}

	// A fresh acquire builds a NEW instance — the doomed one is out of the cache.
	inst2, rel3, err := m.acquire(cfg, testProject("wd"))
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
	m := NewManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "docker", Config: []byte(`{"image":"i"}`)}

	inst, rel, err := m.acquire(cfg, testProject("wd"))
	if err != nil {
		t.Fatal(err)
	}
	rel()

	// Not doomed: the released instance stays cached and is reused.
	inst2, rel2, err := m.acquire(cfg, testProject("wd"))
	if err != nil {
		t.Fatal(err)
	}
	if inst2 != inst {
		t.Fatal("released (but not evicted) instance was not reused")
	}
	rel2()

	m.RemoveInstance(cfg, "wd")
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
	m := NewManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "docker", Config: []byte(`{"image":"i"}`)}

	const n = 8
	insts := make([]*sandboxInstance, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			inst, rel, err := m.acquire(cfg, testProject("wd"))
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
// with the error, and the next acquire dials fresh. (The failing config here
// is a docker one with no image — refused before any daemon contact.)
func TestSandboxManagerFailedBuildRetries(t *testing.T) {
	m := NewManager(t.TempDir())
	bad := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{}`)}
	if _, _, err := m.acquire(bad, testProject("p")); err == nil {
		t.Fatal("imageless docker build succeeded")
	}
	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("failed build left %d placeholders cached", left)
	}
	// Same key, now-valid config: the retry builds.
	good := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}
	inst, rel, err := m.acquire(good, testProject("p"))
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
	m := NewManager(t.TempDir())
	gate := make(chan struct{})
	sb := &closeCountingSandbox{}
	m.buildOverride = func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error) {
		<-gate
		return sb, nil
	}
	return m, gate, sb
}

// A content update while an old-generation build is dialing: the fence dooms
// the instance the moment its build lands — the run that started on it
// finishes and releases, then it closes. It never re-enters the cache for new
// runs.
func TestSandboxManagerRetireFencesInFlightBuilds(t *testing.T) {
	m, gate, sb := gatedManager(t)
	oldCfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`), RuntimeGen: 1}

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(oldCfg, testProject("p"))
		if err != nil {
			t.Error(err)
			return
		}
		rel = r
	}()

	// The update lands mid-dial: generations below 2 are retired.
	m.Retire("sb", 2)
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

// CloseAll while a build is dialing: the placeholder is doomed, the builder's
// freshly dialed resource is closed by the last release — not leaked with no
// owner, which is what deleting the placeholder outright used to do.
func TestSandboxManagerCloseAllDuringBuild(t *testing.T) {
	m, gate, sb := gatedManager(t)
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`), RuntimeGen: 1}

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(cfg, testProject("p"))
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
	if _, _, err := m.acquire(cfg, testProject("p")); err == nil {
		t.Fatal("acquire succeeded on a closed manager")
	}
}

// A rename bumps the row revision but not the runtime generation, and the
// cache keys on the generation: the renamed config keeps sharing the live
// instance, and nothing retires it over a display-name edit.
func TestSandboxManagerRenameSharesInstance(t *testing.T) {
	m := NewManager(t.TempDir())
	sb := &closeCountingSandbox{}
	builds := 0
	m.buildOverride = func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error) {
		builds++
		return sb, nil
	}

	v1 := &store.SandboxConfig{ID: "sb", Name: "old", Type: "docker", Config: []byte(`{"image":"i"}`), Revision: 1, RuntimeGen: 1}
	_, rel1, err := m.Acquire(v1, testProject("p"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := &store.SandboxConfig{ID: "sb", Name: "new", Type: "docker", Config: []byte(`{"image":"i"}`), Revision: 2, RuntimeGen: 1}
	_, rel2, err := m.Acquire(renamed, testProject("p"))
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
	rotated := &store.SandboxConfig{ID: "sb", Name: "new", Type: "docker", Config: []byte(`{"image":"i"}`), Revision: 3, RuntimeGen: 2}
	_, rel3, err := m.Acquire(rotated, testProject("p"))
	if err != nil {
		t.Fatal(err)
	}
	defer rel3()
	if builds != 2 {
		t.Fatalf("builds = %d after a content change, want 2", builds)
	}
}

// A delete landing between a caller's config read and its acquire: the
// tombstone dooms the late build instead of letting it enter the cache as an
// instance of a deleted config that nothing would ever retire.
func TestSandboxManagerRemoveFencesLateAcquires(t *testing.T) {
	m, gate, sb := gatedManager(t)
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`), RuntimeGen: 1}

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(cfg, testProject("p"))
		if err != nil {
			t.Error(err)
			return
		}
		rel = r
	}()

	// The DELETE lands mid-dial: the id is gone for good.
	m.Remove("sb")
	close(gate)
	<-done

	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("a deleted config's build re-entered the cache (%d instances)", left)
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
	m := NewManager(t.TempDir())
	m.SetIdleTimeout(func() time.Duration { return 20 * time.Millisecond })
	closed := &closeCountingSandbox{}
	m.buildOverride = func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error) {
		return closed, nil
	}
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}

	inst, rel, err := m.acquire(cfg, testProject("p"))
	if err != nil {
		t.Fatal(err)
	}
	// Re-acquire inside the idle window disarms the timer.
	rel()
	_, rel2, err := m.acquire(cfg, testProject("p"))
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
	m := NewManager(t.TempDir())
	m.SetIdleTimeout(func() time.Duration { return 5 * time.Millisecond })
	blocking := &blockingCloseSandbox{closing: make(chan struct{}), release: make(chan struct{})}
	fresh := &closeCountingSandbox{}
	first := true
	m.buildOverride = func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error) {
		if first {
			first = false
			return blocking, nil
		}
		return fresh, nil
	}
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}

	old, rel, err := m.acquire(cfg, testProject("p"))
	if err != nil {
		t.Fatal(err)
	}
	rel() // arms the idle timer
	<-blocking.closing

	// The expiry is now mid-stop. A concurrent acquire must block on gone.
	got := make(chan *sandboxInstance, 1)
	go func() {
		inst, r, err := m.acquire(cfg, testProject("p"))
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

// RemoveProject evicts every cached instance keyed to the project — idle ones
// close now — and leaves other projects' instances alone.
func TestSandboxManagerRemoveProject(t *testing.T) {
	m := NewManager(t.TempDir())
	mine := &closeCountingSandbox{}
	other := &closeCountingSandbox{}
	m.buildOverride = func(_ *store.SandboxConfig, p *store.Project) (sandbox.Sandbox, error) {
		if p.ID == "p1" {
			return mine, nil
		}
		return other, nil
	}
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}
	_, r1, err := m.Acquire(cfg, testProject("p1"))
	if err != nil {
		t.Fatal(err)
	}
	r1()
	_, r2, err := m.Acquire(cfg, testProject("p2"))
	if err != nil {
		t.Fatal(err)
	}
	r2()

	m.RemoveProject("p1")
	if mine.closes.Load() != 1 {
		t.Fatalf("removed project's instance closes = %d, want 1", mine.closes.Load())
	}
	if other.closes.Load() != 0 {
		t.Fatal("another project's instance was closed")
	}
	m.mu.Lock()
	_, still := m.instances[sandboxKey{id: "sb", gen: cfg.RuntimeGen, projectID: "p1"}]
	m.mu.Unlock()
	if still {
		t.Fatal("removed project's key still cached")
	}
}
