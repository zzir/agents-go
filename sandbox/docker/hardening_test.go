package docker

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Both container modes must cap the json-file log on the daemon's disk, or a
// flooding command can fill the host filesystem within one timeout window.
func TestBuildHostConfig_LogCapped(t *testing.T) {
	s := &Sandbox{opts: Options{Image: "x"}}
	for name, persistent := range map[string]bool{"ephemeral": false, "persistent": true} {
		t.Run(name, func(t *testing.T) {
			host, _ := s.buildHostConfig(persistent)
			if host.LogConfig.Type != "json-file" {
				t.Errorf("log driver = %q, want json-file", host.LogConfig.Type)
			}
			if got := host.LogConfig.Config["max-size"]; got != logMaxSize {
				t.Errorf("max-size = %q, want %q", got, logMaxSize)
			}
			if got := host.LogConfig.Config["max-file"]; got != "1" {
				t.Errorf("max-file = %q, want %q", got, "1")
			}
		})
	}
}

// The adoption fingerprint: equivalent configurations hash alike, and every
// security-relevant option changes it — that is what stops adoptNamed taking
// over a same-named container created under a laxer policy.
func TestConfigFingerprint(t *testing.T) {
	base := Options{Image: "img", User: "65534:65534", WorkDir: "/srv/data"}
	fp := func(o Options) string { return (&Sandbox{opts: o}).configFingerprint() }

	// Equivalent spellings of the bind source hash alike.
	same := base
	same.WorkDir = "/srv/data/"
	if fp(same) != fp(base) {
		t.Error("equivalent WorkDir spellings produced different fingerprints")
	}
	// The explicit PIDs default and the implicit one are the same container.
	same = base
	same.Limits.PIDs = 128
	if fp(same) != fp(base) {
		t.Error("the default PIDs limit spelled explicitly changed the fingerprint")
	}
	// The default tmpfs size spelled explicitly is the same container.
	same = base
	same.TmpfsSize = "64m"
	if fp(same) != fp(base) {
		t.Error("the default tmpfs size spelled explicitly changed the fingerprint")
	}
	// An empty environment is no environment: both must hash like a config
	// that never mentioned one (see TestConfigFingerprintWithoutEnv).
	same = base
	same.Env = map[string]string{}
	if fp(same) != fp(base) {
		t.Error("an empty Env map changed the fingerprint")
	}
	// Map iteration order is randomized per range, so an unsorted env would
	// hash differently across calls on the same options.
	multi := base
	multi.Env = map[string]string{"A": "1", "B": "2", "C": "3", "D": "4", "E": "5"}
	first := fp(multi)
	for range 20 {
		if fp(multi) != first {
			t.Fatal("the fingerprint of a multi-entry Env is not stable across calls")
		}
	}
	diff := map[string]Options{}
	o := base
	o.Image = "other"
	diff["image"] = o
	o = base
	o.Network = "agents-net"
	diff["network"] = o
	o = base
	o.User = "0:0"
	diff["user"] = o
	o = base
	o.Runtime = "runsc"
	diff["runtime"] = o
	o = base
	o.WorkDir = "/elsewhere"
	diff["workdir"] = o
	o = base
	o.Limits.MemoryBytes = 1 << 30
	diff["memory"] = o
	o = base
	o.Limits.CPUs = 2
	diff["cpus"] = o
	o = base
	o.Limits.PIDs = 64
	diff["pids"] = o
	o = base
	o.WorkDir = ""
	o.VolumeName = "proj-vol"
	diff["volumeName"] = o
	o = base
	o.TmpfsSize = "1g"
	diff["tmpfsSize"] = o
	o = base
	o.Env = map[string]string{"TOKEN": "secret"}
	diff["env"] = o
	for name, opt := range diff {
		if fp(opt) == fp(base) {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
}

// The fingerprint is stamped on the container so adoptNamed can verify it.
func TestPersistentConfig_CarriesFingerprintLabel(t *testing.T) {
	s := &Sandbox{opts: Options{Image: "img", User: "65534:65534"}}
	cfg, _ := s.buildPersistentConfig()
	if got := cfg.Labels[fingerprintLabel]; got != s.configFingerprint() {
		t.Errorf("label = %q, want the config fingerprint %q", got, s.configFingerprint())
	}
}

// An empty User keeps the image's own user — the container is the isolation
// boundary, and one that cannot install a package cannot do the work.
// Persistent mode also relaxes the read-only root fs, which is what makes the
// install possible.
func TestPersistentConfig_EmptyUserKeepsImageDefault(t *testing.T) {
	sb, err := New(Options{Image: "x", Persistent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	cfg, host := sb.buildPersistentConfig()
	if cfg.User != "" {
		t.Errorf("persistent user = %q, want empty (the image's own user)", cfg.User)
	}
	if host.ReadonlyRootfs {
		t.Error("persistent mode should relax the read-only root fs")
	}
}

// An explicit User is applied.
func TestPersistentConfig_ExplicitUser(t *testing.T) {
	sb, err := New(Options{Image: "x", Persistent: true, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	cfg, _ := sb.buildPersistentConfig()
	if cfg.User != "root" {
		t.Errorf("user = %q, want root", cfg.User)
	}
}

// demuxLogs must hand back whatever it collected even when the source dies
// mid-stream with a non-EOF error — the timeout path surfaces that partial
// output to the model.
func TestDemuxLogs_PartialOutputOnError(t *testing.T) {
	var mux strings.Builder
	// One valid stdout frame, then garbage cut off by a failing reader.
	hdr := []byte{1, 0, 0, 0, 0, 0, 0, 5}
	mux.Write(hdr)
	mux.WriteString("hello")

	stdout, stderr, err := demuxLogs(&failAfterReader{data: []byte(mux.String())}, 1<<20)
	if err == nil {
		t.Fatal("expected the injected read error to surface")
	}
	if stdout != "hello" {
		t.Errorf("stdout = %q, want partial output preserved alongside the error", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// failAfterReader yields its data, then fails with a non-EOF error (like a
// force-closed hijacked connection does).
type failAfterReader struct {
	data []byte
	off  int
}

var errReaderClosed = errors.New("use of closed network connection")

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, errReaderClosed
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// The fingerprint of an env-less config is FROZEN: it is stamped on every
// container already running, and a changed hash makes adoptNamed judge them
// all stale — replacing the entire fleet and discarding whatever each had
// installed. Adding an option to configFingerprint must leave this value
// alone; only a deliberate fleet-wide replace may change it. (Last moved when
// Network became a name and the UserUnset flag went away.)
func TestConfigFingerprintWithoutEnv(t *testing.T) {
	const golden = "5698357ac43dfd5d15202737a6f8afba"
	s := &Sandbox{opts: Options{Image: "img", User: "65534:65534", WorkDir: "/srv/data"}}
	if got := s.configFingerprint(); got != golden {
		t.Errorf("fingerprint = %s, want %s — every existing container would be replaced", got, golden)
	}
}

// A container carries the sandbox's environment; a request's entries win for
// the one ephemeral container it creates.
func TestContainerEnv(t *testing.T) {
	s := &Sandbox{opts: Options{Image: "img", Env: map[string]string{"A": "sandbox", "B": "keep"}}}
	got := s.containerEnv(map[string]string{"A": "request", "C": "add"})
	want := []string{"A=request", "B=keep", "C=add"}
	if !slices.Equal(got, want) {
		t.Errorf("containerEnv = %v, want %v", got, want)
	}
	bare := &Sandbox{opts: Options{Image: "img"}}
	if got := bare.containerEnv(map[string]string{"A": "1"}); !slices.Equal(got, []string{"A=1"}) {
		t.Errorf("containerEnv without a sandbox environment = %v, want [A=1]", got)
	}
}
