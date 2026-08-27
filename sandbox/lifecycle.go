package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
)

// The optional capabilities a backend may offer beyond Sandbox itself.
// Callers discover them by type assertion, the way ExecStreamer and
// TerminalOpener are already discovered — a backend that cannot provide one
// simply does not implement it, and no method on Sandbox has to return "not
// supported".

// State is what a sandbox's compute is doing. It says nothing about the
// storage, which outlives every state here.
type State int

const (
	// StateAbsent means nothing is provisioned. The next Start (or the next
	// command) creates it; the working tree is untouched either way.
	StateAbsent State = iota
	// StateStopped means provisioned but not running — the filesystem is intact.
	StateStopped
	// StateRunning means ready to take commands.
	StateRunning
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateRunning:
		return "running"
	default:
		return "absent"
	}
}

// ErrLifecycleUnsupported is returned (wrapped) by a Lifecycle method a
// backend cannot honor in its current configuration — the docker backend
// outside Persistent mode has no long-lived container to stop.
var ErrLifecycleUnsupported = errors.New("sandbox: lifecycle control not supported")

// Lifecycle is implemented by backends whose compute can be started and
// stopped explicitly, rather than only implicitly by the first command.
//
// Stop guarantees exactly one thing: **the filesystem survives**. Whether
// processes do is the backend's business — docker's stop kills them, a
// snapshotting backend may bring them back — so nothing may be written that
// depends on a process outliving a Stop.
type Lifecycle interface {
	// Start provisions the sandbox if needed and makes it ready to take
	// commands. Starting a running sandbox is a no-op.
	Start(ctx context.Context) error
	// Stop releases the compute, keeping the filesystem. Stopping an absent
	// or already-stopped sandbox is a no-op.
	Stop(ctx context.Context) error
	// Status reports what the compute is doing right now.
	Status(ctx context.Context) (State, error)
}

// PortForwarder is implemented by backends that can name where a port inside
// the sandbox is reachable.
type PortForwarder interface {
	// URLForPort returns the base URL a service listening on port inside the
	// sandbox answers at — scheme included, because backends differ: a public
	// HTTPS host on one, a container address on another.
	URLForPort(ctx context.Context, port int) (string, error)
}

// PortDialer is implemented by backends whose ports are NOT reachable from
// wherever the caller runs — a container on a remote daemon — and that can
// open the connection themselves. A caller proxying to URLForPort uses this
// as its transport when the backend offers it.
type PortDialer interface {
	// DialPort opens a connection to a port inside the sandbox.
	DialPort(ctx context.Context, port int) (net.Conn, error)
}

// Exporter is implemented by backends that can hand the working tree back as
// a tar stream — the way files leave a sandbox whose storage the host cannot
// open directly.
type Exporter interface {
	// ExportTar streams path (empty = the working directory) as an
	// uncompressed tar archive. The caller closes the reader.
	ExportTar(ctx context.Context, path string) (io.ReadCloser, error)
}
