package sandboxes

import (
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

func envProject(id, env string) *store.Project {
	p := testProject(id)
	p.Env = env
	p.RuntimeGen = 1
	return p
}

// A project's runtime generation is part of the cache key: an environment
// change must not keep being served from the instance built before it.
func TestSandboxManagerKeysByProjectGeneration(t *testing.T) {
	m := NewManager(t.TempDir())
	m.buildOverride = func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(), nil
	}
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`), RuntimeGen: 1}

	old := envProject("p", "")
	a1, r1, err := m.Acquire(cfg, old)
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	updated := envProject("p", `[{"key":"A","value":"1"}]`)
	updated.RuntimeGen = 2
	a2, r2, err := m.Acquire(cfg, updated)
	if err != nil {
		t.Fatal(err)
	}
	defer r2()
	if a1 == a2 {
		t.Fatal("a new project generation was served the instance built from the old environment")
	}
	// The unchanged generation still hits the cache — a rename (revision
	// only) must not split it.
	again, r3, err := m.Acquire(cfg, envProject("p", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer r3()
	if again != a1 {
		t.Fatal("the same project generation was not served from the cache")
	}
}

// The fence, not just the sweep: an acquire that read the project just before
// the update is still dialing when RetireProject runs, and must not
// repopulate the cache with the old environment.
func TestSandboxManagerRetireProjectFencesInFlightBuilds(t *testing.T) {
	m, gate, sb := gatedManager(t)
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`), RuntimeGen: 1}

	done := make(chan struct{})
	var rel func()
	go func() {
		defer close(done)
		_, r, err := m.acquire(cfg, envProject("p", ""))
		if err != nil {
			t.Error(err)
			return
		}
		rel = r
	}()

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

// Retiring a project leaves the sandbox's OTHER projects alone: they share a
// config, not an environment.
func TestRetireProjectSparesSiblings(t *testing.T) {
	m := NewManager(t.TempDir())
	m.buildOverride = func(*store.SandboxConfig, *store.Project) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(), nil
	}
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`), RuntimeGen: 1}

	mine, r1, err := m.Acquire(cfg, envProject("p1", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	sibling, r2, err := m.Acquire(cfg, envProject("p2", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer r2()

	m.RetireProject("p1", 2)

	stillCached, r3, err := m.Acquire(cfg, envProject("p2", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer r3()
	if stillCached != sibling {
		t.Error("retiring one project evicted another project's instance")
	}
	rebuilt, r4, err := m.Acquire(cfg, envProject("p1", ""))
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
	m := NewManager(t.TempDir())
	cfg := &store.SandboxConfig{ID: "sb", Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}
	_, _, err := m.Acquire(cfg, envProject("p", "{not json"))
	if err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("err = %v, want a project-environment failure", err)
	}
}

// DaemonOptions is the one place a stored config becomes SDK options; the
// container and the tree are the caller's to add.
func TestDaemonOptions(t *testing.T) {
	cfg := &store.SandboxConfig{ID: "sb", Type: "docker", Config: []byte(`{"image":"img","host":"ssh://u@h","memory_mb":256,"cpus":2,"ssh_use_agent":true}`)}
	opts, err := DaemonOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Image != "img" || opts.Host != "ssh://u@h" || opts.Limits.MemoryBytes != 256<<20 || opts.Limits.CPUs != 2 {
		t.Errorf("options = %+v", opts)
	}
	if !opts.SSH.UseAgent {
		t.Error("ssh auth did not carry across")
	}
	if opts.ContainerName != "" || opts.WorkDir != "" || opts.VolumeName != "" || opts.Persistent {
		t.Errorf("options carry container/tree settings that are the caller's: %+v", opts)
	}
	if _, err := DaemonOptions(&store.SandboxConfig{ID: "x", Type: "docker", Config: []byte(`{}`)}); err == nil {
		t.Error("a config without an image was accepted")
	}
	if _, err := DaemonOptions(&store.SandboxConfig{ID: "x", Type: "podman"}); err == nil {
		t.Error("a non-docker type was accepted")
	}
}
