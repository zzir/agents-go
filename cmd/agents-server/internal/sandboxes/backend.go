package sandboxes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// Backend is one sandbox TYPE: the four things only the type knows — build a
// project's sandbox (Open), destroy what it left behind (Reclaim), rebuild the
// compute keeping the storage (Rebuild), and health-check the type (Check).
// Everything a running sandbox can do is on the Sandbox itself and its optional
// capabilities, not here.
//
// Open takes no context deliberately: building a sandbox is CONFIGURATION, not
// I/O. The docker backend dials lazily on the first command; a remote backend
// creates or resumes its instance the same way. That keeps the manager's
// acquire path — which has no request context of its own — unchanged.
type Backend interface {
	// Open builds the Sandbox for spec.
	Open(spec Spec) (sandbox.Sandbox, error)
	// Reclaim destroys the project's compute AND its storage. Called after
	// the row is gone; a failure leaves reclaimable storage rather than a row
	// pointing at nothing (decisions §5.33).
	Reclaim(ctx context.Context, spec Spec) error
	// Rebuild throws the compute away and provisions it again from the
	// current template, KEEPING the storage — the way back from a container
	// someone broke. A backend where the compute IS the storage cannot do
	// that and refuses, rather than quietly destroying a working tree.
	Rebuild(ctx context.Context, spec Spec) error
	// Check reports whether the sandbox is reachable and runnable, without
	// touching any project: the health check behind a Test button. It cleans
	// up whatever it provisioned.
	Check(ctx context.Context, sb *store.Sandbox) error
}

// backends maps a sandbox type to its implementation. A map rather than a
// switch so a build tag or a submodule can register one without editing the
// manager.
var backends = map[string]Backend{
	"docker": dockerBackend{},
	"e2b":    e2bBackend{},
}

// backendFor resolves spec's sandbox type, naming the type when it is unknown
// — a stored row with a type this build does not carry must fail loudly.
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

// RegisterBackend adds a backend for a target type. It panics on a duplicate:
// two implementations of one type is a build mistake, not a runtime condition.
func RegisterBackend(typ string, b Backend) {
	if _, dup := backends[typ]; dup {
		panic("sandboxes: duplicate backend for type " + typ)
	}
	backends[typ] = b
}

// checkHealthCmd is what a Check runs. It needs nothing an image might lack.
var checkHealthCmd = []string{"sh", "-c", "echo ok"}

// checkExec runs the health command and turns a non-zero exit into an error:
// a Check either proves the sandbox usable or says why not.
func checkExec(ctx context.Context, sb sandbox.Sandbox) error {
	runCtx, cancel := context.WithTimeout(ctx, sandbox.DefaultTimeout+5*time.Second)
	defer cancel()
	res, err := sb.Exec(runCtx, sandbox.ExecRequest{Cmd: checkHealthCmd, Timeout: sandbox.DefaultTimeout})
	if err != nil {
		return err
	}
	if res.TimedOut {
		return fmt.Errorf("the health command timed out")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("the health command exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
