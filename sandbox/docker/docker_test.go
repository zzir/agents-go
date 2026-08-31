package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
	if string(host.NetworkMode) != "bridge" {
		t.Errorf("network mode = %q, want the named network when Options.Network is set", host.NetworkMode)
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

// fakeEphemeralDaemon serves the API slice streamEphemeral touches. Its wait
// endpoint never answers, and its log stream ends early with the container
// still "running"; kills are recorded.
func fakeEphemeralDaemon(t *testing.T) (host string, acted *[]string) {
	t.Helper()
	var mu sync.Mutex
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(p, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case strings.HasSuffix(p, "/images/img/json"):
			_, _ = w.Write([]byte("{}"))
		case strings.HasSuffix(p, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"cid"}`))
		case strings.HasSuffix(p, "/containers/cid/archive"),
			strings.HasSuffix(p, "/containers/cid/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/containers/cid/logs"):
			var mux bytes.Buffer
			muxWrite(&mux, stdcopy.Stdout, []byte("early"))
			_, _ = w.Write(mux.Bytes()) // then EOF: the follow broke, the process runs on
		case strings.HasSuffix(p, "/containers/cid/wait"):
			<-r.Context().Done() // answers only when the client gives up
		case strings.HasSuffix(p, "/containers/cid/kill"):
			mu.Lock()
			actions = append(actions, "kill")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String(), &actions
}

// When the follow-log stream ends early with the container still running, the
// wait must ride the request timeout — killing the container and reporting
// TimedOut — never hang on the caller's ctx (spec §2.7m).
func TestStreamEphemeralWaitHonorsTimeout(t *testing.T) {
	host, acted := fakeEphemeralDaemon(t)
	sb, err := New(Options{Host: host, Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	var stdout, stderr strings.Builder
	start := time.Now()
	res, err := sb.ExecStream(t.Context(), sandbox.ExecRequest{
		Cmd:     []string{"sleep", "infinity"},
		Timeout: 300 * time.Millisecond,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("TimedOut = %v, exit = %d; want true, -1", res.TimedOut, res.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ExecStream took %v; the wait was not bounded by the timeout", elapsed)
	}
	if stdout.String() != "early" {
		t.Errorf("stdout = %q, want the pre-break output", stdout.String())
	}
	if !slices.Contains(*acted, "kill") {
		t.Errorf("actions = %v, want the timed-out container killed", *acted)
	}
}

// fakeCorruptLogsDaemon serves execEphemeral's API slice: its wait endpoint
// hangs (waitHangs) or answers exit 0, and its log stream carries one valid
// stdout frame then a corrupt header, so the demux fails mid-stream.
func fakeCorruptLogsDaemon(t *testing.T, waitHangs bool) (host string, acted *[]string) {
	t.Helper()
	var mu sync.Mutex
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(p, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case strings.HasSuffix(p, "/images/img/json"):
			_, _ = w.Write([]byte("{}"))
		case strings.HasSuffix(p, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"cid"}`))
		case strings.HasSuffix(p, "/containers/cid/archive"),
			strings.HasSuffix(p, "/containers/cid/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(p, "/containers/cid/logs"):
			var mux bytes.Buffer
			muxWrite(&mux, stdcopy.Stdout, []byte("started"))
			mux.Write([]byte{9, 0, 0, 0, 0, 0, 0, 1, 'x'}) // unknown stream type: a real demux error
			_, _ = w.Write(mux.Bytes())
		case strings.HasSuffix(p, "/containers/cid/wait"):
			if waitHangs {
				<-r.Context().Done() // answers only when the client gives up
				return
			}
			_, _ = w.Write([]byte(`{"StatusCode":0}`))
		case strings.HasSuffix(p, "/containers/cid/kill"):
			mu.Lock()
			actions = append(actions, "kill")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String(), &actions
}

// A timed-out ephemeral exec whose log read then breaks must still present the
// TimedOut result, carrying whatever output was collected — losing it turned a
// timeout into a bare error.
func TestExecEphemeralTimeoutKeepsPartialLogs(t *testing.T) {
	host, acted := fakeCorruptLogsDaemon(t, true)
	sb, err := New(Options{Host: host, Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	res, err := sb.Exec(t.Context(), sandbox.ExecRequest{
		Cmd:     []string{"sleep", "infinity"},
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("TimedOut = %v, exit = %d; want true, -1", res.TimedOut, res.ExitCode)
	}
	if res.Stdout != "started" {
		t.Errorf("stdout = %q, want the partial output collected before the demux error", res.Stdout)
	}
	if !slices.Contains(*acted, "kill") {
		t.Errorf("actions = %v, want the timed-out container killed", *acted)
	}
}

// Without a timeout the same broken log read stays an error: the exit code is
// fine but the output is not trustworthy, and nothing else needs presenting.
func TestExecEphemeralLogReadFailureIsAnError(t *testing.T) {
	host, _ := fakeCorruptLogsDaemon(t, false)
	sb, err := New(Options{Host: host, Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"true"}}); err == nil ||
		!strings.Contains(err.Error(), "read logs") {
		t.Errorf("Exec err = %v, want the log-read failure surfaced", err)
	}
}

// fakeStoppedDaemon reports the cached container as existing but stopped;
// startStatus is the start endpoint's answer.
func fakeStoppedDaemon(t *testing.T, startStatus int) (host string, acted *[]string) {
	t.Helper()
	var mu sync.Mutex
	var actions []string
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		actions = append(actions, s)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(p, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case strings.HasSuffix(p, "/containers/cid/json"):
			_, _ = w.Write([]byte(`{"Id":"cid","State":{"Running":false}}`))
		case strings.HasSuffix(p, "/containers/cid/start"):
			record("start")
			w.WriteHeader(startStatus)
		case r.Method == http.MethodDelete:
			record("remove")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String(), &actions
}

// A cached container that turns out merely STOPPED (docker stop, an admin
// panel, a daemon restart) is started again in place: remove+recreate would
// destroy its installed packages — and, on an anonymous volume, the whole
// workspace. Only a failed start falls back to remove+recreate.
func TestLookupRunningRestartsStoppedContainer(t *testing.T) {
	t.Run("start succeeds", func(t *testing.T) {
		host, acted := fakeStoppedDaemon(t, http.StatusNoContent)
		sb, err := New(Options{Host: host, Image: "img", Persistent: true})
		if err != nil {
			t.Fatal(err)
		}
		sb.containerID = "cid"
		id, err := sb.lookupRunning(t.Context())
		if err != nil || id != "cid" {
			t.Fatalf("lookupRunning = %q, %v; want cid, nil", id, err)
		}
		if want := []string{"start"}; !slices.Equal(*acted, want) {
			t.Errorf("actions = %v, want %v (no remove)", *acted, want)
		}
		if sb.containerID != "cid" {
			t.Errorf("containerID = %q, want kept", sb.containerID)
		}
	})
	t.Run("start fails", func(t *testing.T) {
		host, acted := fakeStoppedDaemon(t, http.StatusInternalServerError)
		sb, err := New(Options{Host: host, Image: "img", Persistent: true})
		if err != nil {
			t.Fatal(err)
		}
		sb.containerID = "cid"
		id, err := sb.lookupRunning(t.Context())
		if err != nil || id != "" {
			t.Fatalf("lookupRunning = %q, %v; want a forgotten container", id, err)
		}
		if want := []string{"start", "remove"}; !slices.Equal(*acted, want) {
			t.Errorf("actions = %v, want %v", *acted, want)
		}
		if sb.containerID != "" {
			t.Errorf("containerID = %q, want cleared", sb.containerID)
		}
	})
}
