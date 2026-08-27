package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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
	if len(host.Mounts) != 1 || host.Mounts[0].Target != workDir {
		t.Errorf("expected a writable %s volume mount, got %v", workDir, host.Mounts)
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
	s := &Sandbox{opts: Options{Image: "x", Network: "bridge"}}
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
		_, _ = io.Copy(&b, tr)
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

	if !dirs["sub"] {
		t.Errorf("missing parent dir entry for nested file, got %v", dirs)
	}
	if modes["sub"] != 0o777 {
		t.Errorf("nested dir mode = %o, want 777", modes["sub"])
	}
	if files["main.py"] != "print(1)" {
		t.Errorf("main.py = %q", files["main.py"])
	}
	if files["sub/data.txt"] != "hi" {
		t.Errorf("sub/data.txt = %q", files["sub/data.txt"])
	}
	// No path traversal / leading slash.
	for name := range modes {
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			t.Errorf("unsafe tar entry: %q", name)
		}
	}
}

func TestBuildTar_NoFiles(t *testing.T) {
	r, err := buildTar(nil)
	if err != nil {
		t.Fatal(err)
	}
	files, _, dirs := readTar(t, r)
	if len(files) != 0 || len(dirs) != 0 {
		t.Errorf("empty request should produce empty tar, got files=%v dirs=%v", files, dirs)
	}
}

func TestBuildTar_Traversal(t *testing.T) {
	r, err := buildTar(map[string]string{"../escape.py": "x"})
	if err != nil {
		t.Fatal(err)
	}
	files, _, _ := readTar(t, r)
	if files["escape.py"] != "x" {
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
	if got := cfg.Entrypoint; len(got) != 2 || got[0] != "python" || got[1] != "main.py" {
		t.Errorf("entrypoint = %v, want [python main.py]", got)
	}
	if len(cfg.Cmd) != 0 {
		t.Errorf("cmd = %v, want empty (image CMD must not leak in)", cfg.Cmd)
	}
}

// The live attach stream, unlike a finished container log, must be read to its
// end even after both sinks are full: there "no more output wanted" is not
// "the process exited". Ending the read at a full sink handed a still-running
// exec to ExecInspect, which reports ExitCode 0 for it — a command flooding
// both streams was reported as a clean exit while it kept running.
func TestCopyAttached_KeepsReadingWhenSinksAreFull(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		var mux bytes.Buffer
		muxWrite(&mux, stdcopy.Stdout, bytes.Repeat([]byte("o"), 4096))
		muxWrite(&mux, stdcopy.Stderr, bytes.Repeat([]byte("e"), 4096))
		_, _ = pw.Write(mux.Bytes())
		// The writer stays open on purpose: the process is still running, it
		// has merely stopped talking.
	}()

	stdout := &sandbox.CappedBuffer{Max: 8}
	stderr := &sandbox.CappedBuffer{Max: 8}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	severed := errors.New("severed")
	done := make(chan error, 1)
	go func() {
		done <- copyAttached(ctx, pr, func() { _ = pr.CloseWithError(severed) }, stdout, stderr)
	}()

	select {
	case err := <-done:
		t.Fatalf("copyAttached returned (%v) while the stream was still open; a full sink must not end the read", err)
	case <-time.After(250 * time.Millisecond):
	}

	cancel() // only the deadline (or cancellation) may end the read
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("copyAttached did not return after the context fired")
	}
	if stdout.String() != strings.Repeat("o", 8) || stderr.String() != strings.Repeat("e", 8) {
		t.Errorf("stdout = %q, stderr = %q; want each capped at 8 bytes", stdout.String(), stderr.String())
	}
}

// refusingWriter is a sink that rejects every write — a caller's writer whose
// consumer has gone away (a disconnected client, a closed file).
type refusingWriter struct{ err error }

func (w refusingWriter) Write([]byte) (int, error) { return 0, w.err }

// refusingReader ends a stream with the given error, standing in for the
// severed attach connection.
type refusingReader struct{ err error }

func (r refusingReader) Read([]byte) (int, error) { return 0, r.err }

// A copy that ended for a real reason is reported, because on a live attach the
// stream IS the process-lifetime signal: ExecInspect would answer for a process
// that may still be running. Both Exec and ExecStream share this core, so a
// refused write fails the call rather than being dropped. (The ephemeral log
// copy deliberately differs — ContainerWait tells it the exit status
// independently, so there the same failure costs only output bytes.)
func TestCopyAttached_ReportsARefusedWrite(t *testing.T) {
	var mux bytes.Buffer
	muxWrite(&mux, stdcopy.Stdout, []byte("hello"))

	boom := errors.New("boom")
	err := copyAttached(context.Background(), &mux, func() {}, refusingWriter{boom}, io.Discard)
	if !errors.Is(err, boom) {
		t.Errorf("copyAttached err = %v, want the sink's error (%v)", err, boom)
	}
}

// A stream that ends mid-frame is truncation, not failure: the bytes that did
// arrive are the result, and the exit status is still worth reading.
func TestCopyAttached_TruncatedStreamIsNotAnError(t *testing.T) {
	var mux bytes.Buffer
	muxWrite(&mux, stdcopy.Stdout, []byte("hello"))
	torn := io.MultiReader(&mux, refusingReader{io.ErrUnexpectedEOF})

	var out bytes.Buffer
	if err := copyAttached(context.Background(), torn, func() {}, &out, io.Discard); err != nil {
		t.Fatalf("copyAttached err = %v, want nil for a truncated stream", err)
	}
	if out.String() != "hello" {
		t.Errorf("stdout = %q, want the bytes that did arrive", out.String())
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
