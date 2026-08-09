package docker

import (
	"errors"
	"strings"
	"testing"
)

// Both container modes must cap the json-file log on the daemon's disk, or a
// flooding command can fill the host filesystem within one timeout window.
func TestBuildHostConfig_LogCapped(t *testing.T) {
	s := &Sandbox{opts: Options{Image: "x"}}
	for name, persistent := range map[string]bool{"ephemeral": false, "persistent": true} {
		t.Run(name, func(t *testing.T) {
			host := s.buildHostConfig(persistent)
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
	// UserUnset makes any User value moot (the container runs the image
	// default either way), so a leftover User must not split the fingerprint.
	unsetA, unsetB := base, base
	unsetA.UserUnset = true
	unsetB.UserUnset, unsetB.User = true, "leftover:value"
	if fp(unsetA) != fp(unsetB) {
		t.Error("a moot User value under UserUnset changed the fingerprint")
	}

	diff := map[string]Options{}
	o := base
	o.Image = "other"
	diff["image"] = o
	o = base
	o.Network = true
	diff["network"] = o
	o = base
	o.User = "0:0"
	diff["user"] = o
	o = base
	o.UserUnset = true
	diff["userUnset"] = o
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

// New defaults User to 65534:65534 (unless UserUnset), and persistent mode
// must actually apply it — the security default documented on Options.User.
func TestPersistentConfig_DefaultsToNobody(t *testing.T) {
	sb, err := New(Options{Image: "x", Persistent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	cfg, host := sb.buildPersistentConfig()
	if cfg.User != "65534:65534" {
		t.Errorf("persistent user = %q, want the 65534:65534 default", cfg.User)
	}
	if host.ReadonlyRootfs {
		t.Error("persistent mode should relax the read-only root fs")
	}
}

func TestPersistentConfig_UserUnsetKeepsImageDefault(t *testing.T) {
	sb, err := New(Options{Image: "x", Persistent: true, UserUnset: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	cfg, _ := sb.buildPersistentConfig()
	if cfg.User != "" {
		t.Errorf("user = %q, want empty (image default) with UserUnset", cfg.User)
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
