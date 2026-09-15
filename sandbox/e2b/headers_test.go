package e2b_test

import (
	"testing"

	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/e2b"
)

// Options.Headers reach both planes — the control plane (create) and envd (the
// command, the file read) — under the client's own headers, which they cannot
// replace; a service that authenticates with its own header takes nothing else.
func TestHeadersReachBothPlanes(t *testing.T) {
	sb, f := fakeBackedSandboxWith(t, false, func(o *e2b.Options) {
		o.APIKey = "e2b_placeholder"
		o.Headers = map[string]string{"Authorization": "Bearer bailian-key"}
	})
	f.requireHeader = [2]string{"Authorization", "Bearer bailian-key"}
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatalf("exec with the header: %v", err)
	}
	if err := sb.WriteFile(t.Context(), "h.txt", []byte("x")); err != nil {
		t.Fatalf("write with the header: %v", err)
	}
	if _, err := sb.ReadFile(t.Context(), "h.txt"); err != nil {
		t.Fatalf("read with the header: %v", err)
	}

	// Without it the very first control-plane call is refused.
	bare, f2 := fakeBackedSandbox(t)
	f2.requireHeader = [2]string{"Authorization", "Bearer bailian-key"}
	if _, err := bare.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err == nil {
		t.Fatal("a request without the required header was accepted")
	}

	// A same-named entry never replaces a header the client sets itself, on
	// either plane (AuthAPIKey makes envd take the key too).
	own, f3 := fakeBackedSandboxWith(t, false, func(o *e2b.Options) {
		o.DataPlaneAuth = e2b.AuthAPIKey
		o.Headers = map[string]string{"X-API-Key": "override"}
	})
	f3.requireHeader = [2]string{"X-API-Key", "key"}
	if _, err := own.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatalf("Options.Headers replaced the client's own credential: %v", err)
	}
}
