package sandbox

import (
	"context"
	"errors"
	"io"
)

// The optional capabilities a backend may offer beyond Sandbox itself,
// discovered by type assertion the way ExecStreamer and TerminalOpener are.

// State is what a sandbox's compute is doing. The storage outlives every
// state here.
type State int

const (
	// StateAbsent means nothing is provisioned; the next Start or command
	// creates it.
	StateAbsent State = iota
	// StateStopped means provisioned but not running; the filesystem is intact.
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
// backend cannot honor in its current configuration.
var ErrLifecycleUnsupported = errors.New("sandbox: lifecycle control not supported")

// Lifecycle is implemented by backends whose compute can be started and
// stopped explicitly. Stop guarantees exactly one thing — the filesystem
// survives; whether processes do is the backend's business (spec §2.7p).
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

// Exporter is implemented by backends that can hand the working tree back as
// a tar stream — how files leave a sandbox whose storage the host cannot open.
type Exporter interface {
	// ExportTar streams the working directory as an uncompressed tar archive.
	// The caller closes the reader.
	ExportTar(ctx context.Context) (io.ReadCloser, error)
}
