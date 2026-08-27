package sandbox_test

import (
	"testing"

	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/sandboxtest"
)

// LocalSandbox runs the core of the suite: it implements Sandbox and
// ExecStreamer and nothing else, so the Lifecycle, Terminal and Export
// subtests skip themselves. That is the point of a suite that detects
// capabilities rather than demanding them.
func TestLocalSandboxConformance(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T) sandbox.Sandbox {
		t.Helper()
		return sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: t.TempDir()})
	})
}
