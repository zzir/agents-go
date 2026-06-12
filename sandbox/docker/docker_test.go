package docker

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"

	"github.com/zzir/agents-go/sandbox"
)

func TestBuildConfig_SecurityDefaults(t *testing.T) {
	s := &Sandbox{opts: Options{Image: "python:3.12-slim", User: "65534:65534",
		Limits: sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5}}}

	cfg, host := s.buildConfig(sandbox.ExecRequest{Cmd: []string{"python", "main.py"}})

	if cfg.Image != "python:3.12-slim" {
		t.Errorf("image = %q", cfg.Image)
	}
	if cfg.WorkingDir != workDir {
		t.Errorf("workdir = %q", cfg.WorkingDir)
	}
	if cfg.User != "65534:65534" {
		t.Errorf("user = %q", cfg.User)
	}
	if string(host.NetworkMode) != "none" {
		t.Errorf("network = %q, want none", host.NetworkMode)
	}
	if !host.ReadonlyRootfs {
		t.Error("root fs should be read-only")
	}
	if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" {
		t.Errorf("cap drop = %v, want [ALL]", host.CapDrop)
	}
	if host.Memory != 256<<20 {
		t.Errorf("memory = %d", host.Memory)
	}
	if host.NanoCPUs != int64(0.5*1e9) {
		t.Errorf("nanocpus = %d", host.NanoCPUs)
	}
	if host.PidsLimit == nil || *host.PidsLimit != 128 {
		t.Errorf("pids limit = %v, want 128", host.PidsLimit)
	}
	if len(host.Mounts) != 1 || host.Mounts[0].Target != volumeDir {
		t.Errorf("expected a writable %s volume mount, got %v", volumeDir, host.Mounts)
	}
	// With a read-only root fs the container still needs a writable /tmp.
	spec, ok := host.Tmpfs["/tmp"]
	if !ok {
		t.Fatalf("expected a /tmp tmpfs mount, got %v", host.Tmpfs)
	}
	for _, opt := range []string{"rw", "noexec", "mode=1777"} {
		if !strings.Contains(spec, opt) {
			t.Errorf("/tmp tmpfs spec %q missing %q", spec, opt)
		}
	}
}

func TestBuildConfig_NetworkEnabled(t *testing.T) {
	s := &Sandbox{opts: Options{Image: "x", Network: true}}
	_, host := s.buildConfig(sandbox.ExecRequest{Cmd: []string{"x"}})
	if string(host.NetworkMode) == "none" {
		t.Error("network should be enabled when Options.Network is true")
	}
}

// readTar collects header-by-content (files) and header-by-mode (all entries).
func readTar(t *testing.T, r io.Reader) (files map[string]string, modes map[string]int64, dirs map[string]bool) {
	t.Helper()
	files, modes, dirs = map[string]string{}, map[string]int64{}, map[string]bool{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[hdr.Name] = hdr.Mode
		if hdr.Typeflag == tar.TypeDir {
			dirs[hdr.Name] = true
			continue
		}
		var b bytes.Buffer
		io.Copy(&b, tr)
		files[hdr.Name] = b.String()
	}
	return files, modes, dirs
}

func TestBuildTar(t *testing.T) {
	r, err := buildTar(map[string]string{"main.py": "print(1)", "sub/data.txt": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	files, modes, dirs := readTar(t, r)

	// The working directory itself is created world-writable (sticky) so the
	// unprivileged user can write to it despite the root-owned volume root.
	if !dirs["work"] {
		t.Fatalf("missing %q dir entry, got dirs %v", "work", dirs)
	}
	if modes["work"] != 0o1777 {
		t.Errorf("work dir mode = %o, want 1777", modes["work"])
	}
	if !dirs["work/sub"] {
		t.Errorf("missing parent dir entry for nested file, got %v", dirs)
	}
	if modes["work/sub"] != 0o777 {
		t.Errorf("nested dir mode = %o, want 777", modes["work/sub"])
	}
	if files["work/main.py"] != "print(1)" {
		t.Errorf("main.py = %q", files["work/main.py"])
	}
	if files["work/sub/data.txt"] != "hi" {
		t.Errorf("sub/data.txt = %q", files["work/sub/data.txt"])
	}
	// No path traversal / leading slash.
	for name := range modes {
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			t.Errorf("unsafe tar entry: %q", name)
		}
	}
}

func TestBuildTar_NoFilesStillCreatesWorkdir(t *testing.T) {
	r, err := buildTar(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, modes, dirs := readTar(t, r)
	if !dirs["work"] || modes["work"] != 0o1777 {
		t.Errorf("empty request must still create the writable workdir, got %v / %o", dirs, modes["work"])
	}
}

func TestBuildTar_Traversal(t *testing.T) {
	r, err := buildTar(map[string]string{"../escape.py": "x"})
	if err != nil {
		t.Fatal(err)
	}
	files, _, _ := readTar(t, r)
	if files["work/escape.py"] != "x" {
		t.Errorf("traversal should be stripped, got %v", files)
	}
}

// muxWrite appends one frame of the Docker multiplexed log stream: an 8-byte
// header (stream type + big-endian payload length) followed by the payload.
// The writer side of this protocol is no longer exported by the moby client.
func muxWrite(buf *bytes.Buffer, stream stdcopy.StdType, p []byte) {
	hdr := [8]byte{0: byte(stream)}
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(p)))
	buf.Write(hdr[:])
	buf.Write(p)
}

func TestDemuxLogs_CapsEachStream(t *testing.T) {
	var mux bytes.Buffer
	for range 100 {
		muxWrite(&mux, stdcopy.Stdout, bytes.Repeat([]byte("o"), 1024))
		muxWrite(&mux, stdcopy.Stderr, []byte("e"))
	}

	stdout, stderr, err := demuxLogs(&mux, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) != 4096 {
		t.Errorf("stdout = %d bytes, want capped at 4096", len(stdout))
	}
	if len(stderr) == 0 || len(stderr) > 4096 {
		t.Errorf("stderr = %d bytes, want >0 and <= 4096", len(stderr))
	}
}

func TestDemuxLogs_SmallOutputUnchanged(t *testing.T) {
	var mux bytes.Buffer
	muxWrite(&mux, stdcopy.Stdout, []byte("hello"))
	muxWrite(&mux, stdcopy.Stderr, []byte("oops"))

	stdout, stderr, err := demuxLogs(&mux, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "hello" || stderr != "oops" {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

// The full command must run via Entrypoint so an image ENTRYPOINT cannot turn
// req.Cmd into mere arguments, and the image CMD is not appended.
func TestBuildConfig_CommandRunsAsEntrypoint(t *testing.T) {
	s := &Sandbox{opts: Options{Image: "python:3.12-slim"}}
	cfg, _ := s.buildConfig(sandbox.ExecRequest{Cmd: []string{"python", "main.py"}})
	if got := []string(cfg.Entrypoint); len(got) != 2 || got[0] != "python" || got[1] != "main.py" {
		t.Errorf("entrypoint = %v, want [python main.py]", got)
	}
	if len(cfg.Cmd) != 0 {
		t.Errorf("cmd = %v, want empty (image CMD must not leak in)", cfg.Cmd)
	}
}

// A flooding stdout must not starve a later, short stderr: reading stops only
// once BOTH streams are full.
func TestDemuxLogs_LateStderrNotStarved(t *testing.T) {
	var mux bytes.Buffer
	// stdout floods far past its cap first...
	for range 100 {
		muxWrite(&mux, stdcopy.Stdout, bytes.Repeat([]byte("o"), 1024))
	}
	// ...then a short stderr arrives at the very end of the stream.
	muxWrite(&mux, stdcopy.Stderr, []byte("boom"))

	stdout, stderr, err := demuxLogs(&mux, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) != 4096 {
		t.Errorf("stdout len = %d, want capped at 4096", len(stdout))
	}
	if stderr != "boom" {
		t.Errorf("stderr = %q, want %q (must not be starved by stdout volume)", stderr, "boom")
	}
}
