package e2b_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/e2b"
)

// Options.Headers reach both planes: the control plane (create) and envd (the
// command, the file read) — a service that authenticates with its own header
// takes nothing else.
func TestHeadersReachBothPlanes(t *testing.T) {
	root := t.TempDir()
	f := newFakeService(t, root)
	f.requireHeader = [2]string{"Authorization", "Bearer bailian-key"}
	base, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	newSandbox := func(headers map[string]string) *e2b.Sandbox {
		sb, err := e2b.New(e2b.Options{
			APIURL:     f.URL(),
			Domain:     "test",
			APIKey:     "e2b_placeholder",
			TemplateID: "base",
			Headers:    headers,
			HTTPClient: &http.Client{Transport: envdRedirect{to: base, next: http.DefaultTransport}},
			WorkDir:    root,
		})
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}

	sb := newSandbox(map[string]string{"Authorization": "Bearer bailian-key"})
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatalf("exec with the header: %v", err)
	}
	if err := sb.WriteFile(t.Context(), "h.txt", []byte("x")); err != nil {
		t.Fatalf("write with the header: %v", err)
	}
	if _, err := sb.ReadFile(t.Context(), "h.txt"); err != nil {
		t.Fatalf("read with the header: %v", err)
	}

	// Without it the very first control-plane call is refused, and the error
	// carries the service's own message.
	if _, err := newSandbox(nil).Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err == nil {
		t.Fatal("a request without the required header was accepted")
	}
}
