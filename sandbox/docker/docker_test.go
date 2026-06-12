package docker

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/pkg/stdcopy"

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

func TestDemuxLogs_CapsEachStream(t *testing.T) {
	var mux bytes.Buffer
	outW := stdcopy.NewStdWriter(&mux, stdcopy.Stdout)
	errW := stdcopy.NewStdWriter(&mux, stdcopy.Stderr)
	for range 100 {
		outW.Write(bytes.Repeat([]byte("o"), 1024))
		errW.Write([]byte("e"))
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
	stdcopy.NewStdWriter(&mux, stdcopy.Stdout).Write([]byte("hello"))
	stdcopy.NewStdWriter(&mux, stdcopy.Stderr).Write([]byte("oops"))

	stdout, stderr, err := demuxLogs(&mux, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "hello" || stderr != "oops" {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}
}
