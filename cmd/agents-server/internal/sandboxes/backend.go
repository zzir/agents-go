package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// Backend is one sandbox TYPE: build a project's sandbox (Open), destroy what
// it left behind (Reclaim), rebuild the compute keeping the storage (Rebuild)
// and health-check the type (Check). Open is configuration, not I/O: backends
// dial on the first command.
type Backend interface {
	// Open builds the Sandbox for spec.
	Open(spec Spec) (sandbox.Sandbox, error)
	// Reclaim destroys the project's compute AND its storage, after the row
	// is gone — decisions §5.33.
	Reclaim(ctx context.Context, spec Spec) error
	// Rebuild replaces the compute from the current template, KEEPING the
	// storage; a backend where the compute IS the storage refuses (invariant 44).
	Rebuild(ctx context.Context, spec Spec) error
	// Check reports whether the sandbox is reachable and runnable, touching no
	// project and cleaning up whatever it provisioned.
	Check(ctx context.Context, sb *store.Sandbox) error
}

// backends maps a sandbox type to its implementation.
var backends = map[string]Backend{
	"docker": dockerBackend{},
	"e2b":    e2bBackend{},
}

// backendFor resolves spec's sandbox type, naming an unknown type in the error.
func backendFor(spec Spec) (Backend, error) {
	return BackendFor(spec.Sandbox.Type)
}

// BackendFor resolves one sandbox type.
func BackendFor(typ string) (Backend, error) {
	b, ok := backends[typ]
	if !ok {
		return nil, fmt.Errorf("unknown sandbox type: %s", typ)
	}
	return b, nil
}

// checkHealthCmd is what a Check runs. It needs nothing an image might lack.
var checkHealthCmd = []string{"sh", "-c", "echo ok"}

// ErrHealthCommandFailed marks a health check whose service was reached but
// whose command timed out or exited non-zero; the handler answers 200 ok=false
// (502 is for an unreachable service).
var ErrHealthCommandFailed = errors.New("the health command ran and did not succeed")

// checkExec runs the health command and turns a non-zero exit into an error:
// a Check either proves the sandbox usable or says why not.
func checkExec(ctx context.Context, sb sandbox.Sandbox) error {
	runCtx, cancel := context.WithTimeout(ctx, sandbox.DefaultTimeout+5*time.Second)
	defer cancel()
	res, err := sb.Exec(runCtx, sandbox.ExecRequest{Cmd: checkHealthCmd, Timeout: sandbox.DefaultTimeout})
	if err != nil {
		return err // unreachable: the service could not run the command
	}
	if res.TimedOut {
		return fmt.Errorf("%w: it timed out", ErrHealthCommandFailed)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w: exited %d: %s", ErrHealthCommandFailed, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
