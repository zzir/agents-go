//go:build docker_integration

// Run with: go test -tags docker_integration ./sandbox/docker
package docker

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/sandboxtest"
)

// The docker backend in the shape the workbench uses it: a persistent,
// named container over a named volume. That configuration is the one that
// implements every optional capability, so the whole suite runs.
func TestDockerSandboxConformance(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T) sandbox.Sandbox {
		t.Helper()
		name := "agents-conformance-" + t.Name()
		sb, err := New(Options{
			Image:         "alpine:3.20",
			Persistent:    true,
			ContainerName: sanitizeName(name),
			VolumeName:    sanitizeName("agents-vol-" + name),
			User:          "root",
		})
		if err != nil {
			t.Skipf("docker unavailable: %v", err)
			return nil
		}
		t.Cleanup(func() {
			// context.Background(), not t.Context(): the test's context is
			// already cancelled by the time cleanups run, and a cancelled
			// remove leaves the volume behind — which the NEXT run would
			// then reuse, turning "create exclusive" into a false failure.
			ctx, opts := context.Background(), Options{}
			_ = sb.Close()
			_ = RemoveManaged(ctx, opts, sanitizeName(name))
			_ = RemoveManagedVolume(ctx, opts, sanitizeName("agents-vol-"+name))
		})
		return sb
	})
}

// sanitizeName makes a docker-legal name out of a test name (which carries
// slashes and capitals).
func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
