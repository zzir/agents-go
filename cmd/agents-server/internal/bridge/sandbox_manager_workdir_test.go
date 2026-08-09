package bridge

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// One live instance per (config id, workdir): sessions bound to different
// workdirs on the same config must not share a sandbox, while the same pair
// keeps hitting the cache. Remove tears down every variant of the id.
func TestSandboxManagerKeysByWorkdir(t *testing.T) {
	m := NewSandboxManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "local"}

	a1, r1, err := m.Acquire(cfg, "/wd1")
	if err != nil {
		t.Fatalf("wd1: %v", err)
	}
	defer r1()
	a2, r2, err := m.Acquire(cfg, "/wd2")
	if err != nil {
		t.Fatalf("wd2: %v", err)
	}
	defer r2()
	if a1 == a2 {
		t.Fatal("different workdirs share one sandbox instance")
	}
	again, r3, err := m.Acquire(cfg, "/wd1")
	if err != nil {
		t.Fatalf("wd1 again: %v", err)
	}
	defer r3()
	if again != a1 {
		t.Fatal("same (id, workdir) pair not served from the cache")
	}
	// Trim is part of the key: "  /wd1 " is the same instance as "/wd1".
	trimmed, r4, err := m.Acquire(cfg, "  /wd1 ")
	if err != nil {
		t.Fatalf("trimmed wd1: %v", err)
	}
	defer r4()
	if trimmed != a1 {
		t.Fatal("untrimmed workdir minted a duplicate instance")
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
	m := NewSandboxManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "local"}

	inst1, rel1, err := m.acquire(cfg, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	_, rel2, err := m.acquire(cfg, "/wd")
	if err != nil {
		t.Fatal(err)
	}

	m.RemoveInstance(cfg, "/wd")
	m.mu.Lock()
	doomed, refs := inst1.doomed, inst1.refs
	m.mu.Unlock()
	if !doomed || refs != 2 {
		t.Fatalf("after eviction with holders: doomed=%v refs=%d, want doomed with 2 refs", doomed, refs)
	}

	// A fresh acquire builds a NEW instance — the doomed one is out of the cache.
	inst2, rel3, err := m.acquire(cfg, "/wd")
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
	m := NewSandboxManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "local"}

	inst, rel, err := m.acquire(cfg, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	rel()

	// Not doomed: the released instance stays cached and is reused.
	inst2, rel2, err := m.acquire(cfg, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	if inst2 != inst {
		t.Fatal("released (but not evicted) instance was not reused")
	}
	rel2()

	m.RemoveInstance(cfg, "/wd")
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
	m := NewSandboxManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "loc", Name: "local", Type: "local"}

	const n = 8
	insts := make([]*sandboxInstance, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			inst, rel, err := m.acquire(cfg, "/wd")
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
	m := NewSandboxManager(t.TempDir())
	bad := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{}`)}
	if _, _, err := m.acquire(bad, ""); err == nil {
		t.Fatal("imageless docker build succeeded")
	}
	m.mu.Lock()
	left := len(m.instances)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("failed build left %d placeholders cached", left)
	}
	// Same key, now-valid config: the retry builds.
	good := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "local"}
	inst, rel, err := m.acquire(good, "")
	if err != nil {
		t.Fatalf("retry after a failed build: %v", err)
	}
	defer rel()
	if inst.sb == nil {
		t.Fatal("retry returned a sandbox-less instance")
	}
}

// Docker's per-session workdir is the CONTAINER-side directory: persistent
// containers accept /workspace subtrees (/workspace itself normalizes to "",
// the default instance) and pass anything else through for the SDK to reject;
// ephemeral containers ignore the value entirely. local/ssh use it verbatim.
func TestEffectiveWorkDirPerType(t *testing.T) {
	persistent := &store.SandboxConfig{Type: "docker", Config: []byte(`{"image":"i","persistent":true}`)}
	ephemeral := &store.SandboxConfig{Type: "docker", Config: []byte(`{"image":"i"}`)}
	ssh := &store.SandboxConfig{Type: "ssh"}
	local := &store.SandboxConfig{Type: "local"}

	if got := effectiveWorkDir(persistent, "/workspace/proj"); got != "/workspace/proj" {
		t.Fatalf("persistent subdir = %q, want kept", got)
	}
	if got := effectiveWorkDir(persistent, "/workspace"); got != "" {
		t.Fatalf("persistent /workspace = %q, want normalized to \"\"", got)
	}
	// A binding legal when written can be out-of-tree by run time (a config
	// identity update landing beside the bind) — it falls back to the default
	// instance instead of tripping the SDK validation forever.
	if got := effectiveWorkDir(persistent, "/tmp/test"); got != "" {
		t.Fatalf("persistent out-of-workspace = %q, want normalized to \"\"", got)
	}
	if got := effectiveWorkDir(persistent, "/workspace/../etc"); got != "" {
		t.Fatalf("persistent escaping path = %q, want normalized to \"\"", got)
	}
	if got := effectiveWorkDir(ephemeral, "/workspace/proj"); got != "" {
		t.Fatalf("ephemeral workdir = %q, want \"\"", got)
	}
	if got := effectiveWorkDir(ssh, " /y "); got != "/y" {
		t.Fatalf("ssh workdir = %q, want trimmed /y", got)
	}
	if got := effectiveWorkDir(local, ""); got != "" {
		t.Fatalf("local empty workdir = %q, want \"\"", got)
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
func gatedManager(t *testing.T) (*SandboxManager, chan struct{}, *closeCountingSandbox) {
	t.Helper()
	m := NewSandboxManager(t.TempDir())
	gate := make(chan struct{})
	sb := &closeCountingSandbox{}
	m.buildOverride = func(*store.SandboxConfig, string) (sandbox.Sandbox, error) {
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
	oldCfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "local", RuntimeGen: 1}

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(oldCfg, "")
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
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "local", RuntimeGen: 1}

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(cfg, "")
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
	if _, _, err := m.acquire(cfg, ""); err == nil {
		t.Fatal("acquire succeeded on a closed manager")
	}
}

// A rename bumps the row revision but not the runtime generation, and the
// cache keys on the generation: the renamed config keeps sharing the live
// instance, and nothing retires it over a display-name edit.
func TestSandboxManagerRenameSharesInstance(t *testing.T) {
	m := NewSandboxManager(t.TempDir())
	sb := &closeCountingSandbox{}
	builds := 0
	m.buildOverride = func(*store.SandboxConfig, string) (sandbox.Sandbox, error) {
		builds++
		return sb, nil
	}

	v1 := &store.SandboxConfig{ID: "sb", Name: "old", Type: "local", Revision: 1, RuntimeGen: 1}
	_, rel1, err := m.Acquire(v1, "")
	if err != nil {
		t.Fatal(err)
	}
	renamed := &store.SandboxConfig{ID: "sb", Name: "new", Type: "local", Revision: 2, RuntimeGen: 1}
	_, rel2, err := m.Acquire(renamed, "")
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
	rotated := &store.SandboxConfig{ID: "sb", Name: "new", Type: "local", Revision: 3, RuntimeGen: 2}
	_, rel3, err := m.Acquire(rotated, "")
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
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "local", RuntimeGen: 1}

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(cfg, "")
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
