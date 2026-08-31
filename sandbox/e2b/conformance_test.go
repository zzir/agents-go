package e2b_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/e2b"
	"github.com/zzir/agents-go/sandbox/sandboxtest"
)

// envdRedirect points the client's per-sandbox hosts
// ("https://49983-sb1.test") at the fake service, without letting the client
// skip the host-building code that would otherwise go untested.
type envdRedirect struct {
	to   *url.URL
	next http.RoundTripper
}

func (t envdRedirect) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Host, ".test") {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = t.to.Scheme, t.to.Host
		r.Host = t.to.Host
	}
	return t.next.RoundTrip(r)
}

func fakeBackedSandbox(t *testing.T) (*e2b.Sandbox, *fakeService) {
	t.Helper()
	return fakeBackedSandboxWith(t, false)
}

// fakeBackedSandboxWith chooses how the fake renders the protobuf: E2B 0.7's
// spelling, or the older numeric one.
func fakeBackedSandboxWith(t *testing.T, numericEnums bool) (*e2b.Sandbox, *fakeService) {
	t.Helper()
	root := t.TempDir()
	f := newFakeService(t, root)
	f.numericEnums = numericEnums
	base, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := e2b.New(e2b.Options{
		APIURL:     f.URL(),
		Domain:     "test",
		APIKey:     "key",
		TemplateID: "base",
		HTTPClient: &http.Client{Transport: envdRedirect{to: base, next: http.DefaultTransport}},
		// The fake serves one directory as the sandbox's filesystem, so the
		// working directory is that directory.
		WorkDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sb, f
}

// The same suite against a service that renders the protobuf the OLDER way —
// FileType as the enum's number, int64 as a number. Alibaba Cloud's envd 0.5
// does exactly this, and a client that only reads E2B 0.7's spelling fails
// every directory listing against it (found by the protocol probe).
func TestE2BConformanceNumericEnums(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T) sandbox.Sandbox {
		t.Helper()
		sb, _ := fakeBackedSandboxWith(t, true)
		return sb
	})
}

// The whole suite against a service that speaks the wire this client writes:
// the Connect framing, the base64 payloads, the protojson shapes and the
// Sandbox contract, end to end. What it does NOT prove is that the real
// services agree with this reading of their API — that is the protocol
// probe's job (decisions §5.34).
func TestE2BConformance(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T) sandbox.Sandbox {
		t.Helper()
		sb, _ := fakeBackedSandbox(t)
		return sb
	})
}

// One client provisions ONE sandbox, however many commands it runs: a second
// create would be a second billed instance, and a project's tree would split
// in two.
func TestE2BProvisionsOnce(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	for range 3 {
		if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sb.WriteFile(t.Context(), "x.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.createCalls
	f.mu.Unlock()
	if got != 1 {
		t.Fatalf("creates = %d, want 1", got)
	}
}

// A created sandbox is handed to the owner before anything else happens: a
// create the caller cannot record is billed compute nobody will ever stop, so
// the failure is fatal rather than logged.
func TestE2BRecordsTheSandboxID(t *testing.T) {
	root := t.TempDir()
	f := newFakeService(t, root)
	base, _ := url.Parse(f.URL())
	var recorded string
	sb, err := e2b.New(e2b.Options{
		APIURL: f.URL(), Domain: "test", APIKey: "key", TemplateID: "base", WorkDir: root,
		HTTPClient:  &http.Client{Transport: envdRedirect{to: base, next: http.DefaultTransport}},
		OnSandboxID: func(_ context.Context, id string) error { recorded = id; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	if recorded == "" {
		t.Fatal("the sandbox id was never handed back")
	}
	if got, err := sb.Status(t.Context()); err != nil || got != sandbox.StateRunning {
		t.Fatalf("Status = %v, %v; want running", got, err)
	}
}

// Stop pauses and Status reports it; Start resumes, and the tree is intact —
// the one thing Stop promises (spec §2.7p).
func TestE2BStopStartKeepsTheTree(t *testing.T) {
	sb, _ := fakeBackedSandbox(t)
	if err := sb.WriteFile(t.Context(), "kept.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}
	if err := sb.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := sb.Status(t.Context()); got != sandbox.StateStopped {
		t.Fatalf("Status after Stop = %v, want stopped", got)
	}
	if err := sb.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := sb.Status(t.Context()); got != sandbox.StateRunning {
		t.Fatalf("Status after Start = %v, want running", got)
	}
	if data, err := sb.ReadFile(t.Context(), "kept.txt"); err != nil || string(data) != "kept" {
		t.Fatalf("file after a pause/resume = %q, %v", data, err)
	}
}

// Destroy kills the sandbox; afterwards the client provisions a new one
// rather than failing every command against a dead id.
func TestE2BDestroyThenReprovision(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	if err := sb.Destroy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := sb.Status(t.Context()); got != sandbox.StateAbsent {
		t.Fatalf("Status after Destroy = %v, want absent", got)
	}
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.createCalls
	f.mu.Unlock()
	if got != 2 {
		t.Fatalf("creates = %d, want 2 — a destroyed sandbox is replaced, not mourned", got)
	}
}

// Closing an export's reader aborts the stream: the write error reaches the
// chunk callback, the client stops consuming, and the abandoned tar process is
// killed — not streamed to nobody for the rest of the export's timeout.
func TestE2BExportReaderCloseAbortsTheStream(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	// More output than one pipe hand-off: io.Pipe is unbuffered, so the export
	// goroutine is mid-Write when the reader closes.
	if err := sb.WriteFile(t.Context(), "big.bin", bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	rc, err := sb.ExportTar(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	// The abort is visible as the kill of the abandoned tar process.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := f.signalCalls
		f.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the export stream was not aborted after the reader closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// timeoutCalls snapshots the TTLs the fake's /timeout endpoint was asked for.
func timeoutCalls(f *fakeService) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.timeoutCalls)
}

// leasedAtLeast reports whether some call in calls asked for at least secs.
func leasedAtLeast(calls []int, secs int) bool {
	return slices.ContainsFunc(calls, func(v int) bool { return v >= secs })
}

// An export can outrun the default lease: the lease is extended to at least
// the export's own bound before the tar starts.
func TestE2BExportExtendsTheLease(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	// Provision first, so the export's ensure takes the refresh path.
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	rc, err := sb.ExportTar(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if calls := timeoutCalls(f); !leasedAtLeast(calls, 600) {
		t.Fatalf("no refresh covered the 600s export bound; /timeout calls = %v", calls)
	}
}

// An exec bounded past the lease's refresh margin extends the lease to cover
// its own deadline first.
func TestE2BLongExecExtendsTheLease(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}, Timeout: 20 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if calls := timeoutCalls(f); !leasedAtLeast(calls, 1200) {
		t.Fatalf("no refresh covered the 20m exec deadline; /timeout calls = %v", calls)
	}
}

// A terminal session is open-ended and there is no keepalive: opening one at
// least starts it from a freshly refreshed full lease.
func TestE2BOpenTerminalRefreshesTheLease(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	term, err := sb.OpenTerminal(t.Context(), sandbox.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if calls := timeoutCalls(f); !leasedAtLeast(calls, e2b.DefaultTimeout) {
		t.Fatalf("opening a terminal did not refresh the lease; /timeout calls = %v", calls)
	}
	// End the shell: the fake's SendSignal is a stub, and a lingering process
	// would hold its stream request open past the server's Close.
	if _, err := term.Write([]byte("exit 0\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, term)
	_, _ = term.Wait()
}

// deadlineRecorder notes whether the control-plane DELETE — ensure's rollback
// kill — carried a deadline. It runs while s.mu is held, so an unbounded one
// against a hung control plane would wedge every future call.
type deadlineRecorder struct {
	next http.RoundTripper

	mu                sync.Mutex
	sawDelete         bool
	deleteHadDeadline bool
}

func (d *deadlineRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodDelete {
		_, ok := r.Context().Deadline()
		d.mu.Lock()
		d.sawDelete, d.deleteHadDeadline = true, ok
		d.mu.Unlock()
	}
	return d.next.RoundTrip(r)
}

func TestE2BRollbackKillCarriesADeadline(t *testing.T) {
	root := t.TempDir()
	f := newFakeService(t, root)
	base, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	rec := &deadlineRecorder{next: envdRedirect{to: base, next: http.DefaultTransport}}
	sb, err := e2b.New(e2b.Options{
		APIURL: f.URL(), Domain: "test", APIKey: "key", TemplateID: "base", WorkDir: root,
		HTTPClient:  &http.Client{Transport: rec},
		OnSandboxID: func(context.Context, string) error { return errors.New("no record") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err == nil {
		t.Fatal("a create nobody could record must fail")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.sawDelete {
		t.Fatal("the rollback never killed the sandbox")
	}
	if !rec.deleteHadDeadline {
		t.Fatal("the rollback kill ran without a deadline")
	}
}

// A MakeDir failure whose MESSAGE mentions "exists" but whose code is not
// already_exists is a real failure, not an existing directory: it must
// propagate rather than being sniffed into success.
func TestE2BMakeDirFailureMentioningExistsPropagates(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.makeDirHook = func(string) (string, string) { return "internal", "a lease on the path exists" }
	f.mu.Unlock()
	if err := sb.WriteFile(t.Context(), "deep/dir/file.txt", []byte("x")); err == nil {
		t.Fatal("a MakeDir failure whose message mentions \"exists\" was treated as success")
	}
}

// A kill that fails transiently leaves the id remembered, so Destroy can be
// retried; forgetting first would make the retry a no-op and leak the billed
// sandbox.
func TestE2BDestroyRetriesAfterAFailedKill(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failDeletes = 1
	f.mu.Unlock()
	if err := sb.Destroy(t.Context()); err == nil {
		t.Fatal("a Destroy whose kill failed must report it")
	}
	if f.only() == nil {
		t.Fatal("the fake deleted the sandbox despite the injected failure")
	}
	if err := sb.Destroy(t.Context()); err != nil {
		t.Fatalf("the retried Destroy: %v", err)
	}
	if f.only() != nil {
		t.Fatal("the retried Destroy never issued the DELETE")
	}
}

// Content far past Linux's ~128KB per-argument cap goes through: the argv
// carries only the exclusive create, never the content.
func TestE2BCreateExclusiveLargeContent(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	content := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuv"), 7000) // 224KB
	if err := sb.CreateExclusive(t.Context(), "big.bin", content); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(f.root, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %d bytes and differs, want the %d written", len(got), len(content))
	}
	if err := sb.CreateExclusive(t.Context(), "big.bin", []byte("clobber")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second CreateExclusive = %v, want fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "big.bin")); !bytes.Equal(got, content) {
		t.Fatal("a refused create modified the file")
	}
}

// The first-use mkdir completes exactly once before any command proceeds: a
// concurrent first command must wait for /workspace, not run without it.
func TestE2BConcurrentFirstCommandsWaitForTheWorkDir(t *testing.T) {
	root := t.TempDir()
	f := newFakeService(t, root)
	base, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := e2b.New(e2b.Options{
		APIURL: f.URL(), Domain: "test", APIKey: "key", TemplateID: "base",
		HTTPClient: &http.Client{Transport: envdRedirect{to: base, next: http.DefaultTransport}},
		// A directory the template did not ship; the fake fails a command
		// whose cwd is missing, like real envd.
		WorkDir: filepath.Join(root, "workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stall the mkdir so a racing command would run before it completed.
	f.mu.Lock()
	f.makeDirHook = func(string) (string, string) { time.Sleep(100 * time.Millisecond); return "", "" }
	f.mu.Unlock()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}})
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("a concurrent first command ran before the workdir existed: %v", err)
		}
	}
}

// A stock template has no /workspace: the client makes its working directory
// on the sandbox it created, or every command fails with "cwd does not exist"
// (seen on Alibaba Cloud's `base`).
func TestE2BMakesTheWorkDir(t *testing.T) {
	root := t.TempDir()
	f := newFakeService(t, root)
	base, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := e2b.New(e2b.Options{
		APIURL: f.URL(), Domain: "test", APIKey: "key", TemplateID: "base",
		HTTPClient: &http.Client{Transport: envdRedirect{to: base, next: http.DefaultTransport}},
		// A directory the template did not ship.
		WorkDir: filepath.Join(root, "workspace"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(t.Context(), "hello.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspace", "hello.txt")); err != nil {
		t.Fatal(err)
	}
}
