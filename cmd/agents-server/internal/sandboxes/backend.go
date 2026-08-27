package sandboxes

import (
	"context"
	"fmt"

	"github.com/zzir/agents-go/sandbox"
)

// Backend is one sandbox TYPE: how to build a project's sandbox, and how to
// destroy what it left behind. Everything else a project's sandbox can do is
// on the Sandbox itself (and its optional capabilities), so this stays at two
// methods — the two things only the type knows.
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
}

// backends maps a target type to its implementation. A map rather than a
// switch so a build tag or a submodule can register one without editing the
// manager.
var backends = map[string]Backend{
	"docker": dockerBackend{},
}

// backendFor resolves spec's target type, naming the type when it is unknown —
// a stored row with a type this build does not carry must fail loudly.
func backendFor(spec Spec) (Backend, error) {
	b, ok := backends[spec.Target.Type]
	if !ok {
		return nil, fmt.Errorf("unknown sandbox target type: %s", spec.Target.Type)
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
