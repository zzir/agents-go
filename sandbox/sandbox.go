// Package sandbox runs untrusted, agent-generated code in an isolated
// environment and exposes it to an agent as a tool. The Sandbox interface is
// backend-agnostic; concrete backends (Docker, Kubernetes Jobs) live in
// subpackages so that callers who do not use them pull no heavy dependencies.
//
// A sandbox executes a command in a working directory after writing the request
// files into it. Backends enforce isolation (no network, read-only root,
// dropped capabilities, resource and time limits) by default.
package sandbox

import (
	"context"
	"time"
)

// DefaultTimeout bounds an execution when ExecRequest.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// DefaultMaxOutputBytes caps each captured output stream when
// ExecRequest.MaxOutputBytes is zero.
const DefaultMaxOutputBytes int64 = 1 << 20 // 1 MiB

// Sandbox executes commands in an isolated environment.
type Sandbox interface {
	// Exec writes req.Files into a fresh working directory, runs req.Cmd there,
	// and returns the captured output, with each output stream capped at
	// req.MaxOutputBytes (excess output is discarded). Implementations apply
	// isolation and the request timeout, killing the process when it is
	// exceeded.
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	// Close releases any resources held by the sandbox.
	Close() error
}

// ExecRequest describes one execution.
type ExecRequest struct {
	// Cmd is the command and arguments to run in the working directory.
	Cmd []string
	// Files maps a path (relative to the working directory) to its content; they
	// are written before Cmd runs.
	Files map[string]string
	// Stdin is fed to the process's standard input. Only LocalSandbox supports
	// it; the docker and k8s backends reject requests that set it.
	Stdin string
	// Env sets environment variables for the process. Backends do not pass the
	// host environment through: LocalSandbox provides only a minimal set of
	// host variables by default (see LocalOptions.InheritHostEnv), and the
	// container backends start from the image environment.
	Env map[string]string
	// Timeout bounds the execution; zero means DefaultTimeout.
	Timeout time.Duration
	// MaxOutputBytes caps how many bytes of each output stream are kept;
	// excess output is discarded, not buffered. Zero means
	// DefaultMaxOutputBytes.
	MaxOutputBytes int64
}

// EffectiveTimeout returns the request timeout or DefaultTimeout when unset.
func (r ExecRequest) EffectiveTimeout() time.Duration {
	if r.Timeout <= 0 {
		return DefaultTimeout
	}
	return r.Timeout
}

// EffectiveMaxOutputBytes returns the request's per-stream output cap or
// DefaultMaxOutputBytes when unset.
func (r ExecRequest) EffectiveMaxOutputBytes() int64 {
	if r.MaxOutputBytes <= 0 {
		return DefaultMaxOutputBytes
	}
	return r.MaxOutputBytes
}

// ExecResult is the outcome of an execution.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut reports whether the process was killed for exceeding the timeout.
	TimedOut bool
}

// Limits are the resource bounds a backend should enforce. Zero values select
// the backend's default.
type Limits struct {
	// MemoryBytes caps memory; e.g. 256 << 20 for 256 MiB.
	MemoryBytes int64
	// CPUs caps CPU cores (fractional allowed, e.g. 0.5).
	CPUs float64
	// PIDs caps the number of processes/threads.
	PIDs int64
}
