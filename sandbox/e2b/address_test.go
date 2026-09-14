package e2b_test

import (
	"errors"
	"testing"

	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/e2b"
)

// Address is a read: nothing before a sandbox exists, then the id and the
// domain the service returned over the configured one.
func TestAddressReadsTheServiceDomain(t *testing.T) {
	sb, f := fakeBackedSandbox(t)
	if _, _, err := sb.Address(t.Context()); !errors.Is(err, e2b.ErrNotProvisioned) {
		t.Fatalf("before provisioning: %v, want ErrNotProvisioned", err)
	}
	if f.createCalls != 0 {
		t.Fatal("Address provisioned a sandbox")
	}
	f.domain = "sandboxes.example"
	if _, err := sb.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatal(err)
	}
	id, domain, err := sb.Address(t.Context())
	if err != nil || id == "" || domain != "sandboxes.example" {
		t.Fatalf("Address = %q, %q, %v", id, domain, err)
	}
}
