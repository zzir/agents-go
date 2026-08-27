package e2b_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
	root := t.TempDir()
	f := newFakeService(t, root)
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

// A port's public host is built from the sandbox id and the domain, with no
// call of its own beyond provisioning.
func TestE2BHostForPort(t *testing.T) {
	sb, _ := fakeBackedSandbox(t)
	host, err := sb.HostForPort(t.Context(), 3000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(host, "3000-sb") || !strings.HasSuffix(host, ".test") {
		t.Fatalf("host = %q, want 3000-<id>.test", host)
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
