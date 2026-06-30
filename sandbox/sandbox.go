// Package sandbox runs untrusted, agent-generated code in an isolated
// environment and exposes it to an agent as a tool. The Sandbox interface is
// backend-agnostic; the Docker backend lives in a subpackage so that callers
// who do not use it pull no heavy dependencies.
//
// A sandbox executes a command in a working directory after writing the request
// files into it. Backends enforce isolation (no network, read-only root,
// dropped capabilities, resource and time limits) by default.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"
)

// DefaultTimeout bounds an execution when ExecRequest.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// DefaultMaxOutputBytes caps each captured output stream when
// ExecRequest.MaxOutputBytes is zero.
const DefaultMaxOutputBytes int64 = 1 << 20 // 1 MiB

// Sandbox executes commands and performs file operations in an isolated
// environment.
type Sandbox interface {
	// Exec writes req.Files into the working directory, runs req.Cmd there,
	// and returns the captured output capped at req.MaxOutputBytes.
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	// ReadFile reads a file from the sandbox's persistent working directory.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes a file in the sandbox, creating parent directories.
	WriteFile(ctx context.Context, path string, content []byte) error
	// ListDir lists entries in a sandbox directory (empty path = working dir).
	ListDir(ctx context.Context, path string) ([]DirEntry, error)
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
	// Stdin is fed to the process's standard input. LocalSandbox and the SSH
	// backend support it; the docker backend rejects requests that set it.
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

// CappedBuffer is an io.Writer that keeps at most Max bytes and silently
// discards the rest, so a runaway process cannot exhaust memory.
type CappedBuffer struct {
	Buf bytes.Buffer
	Max int64
}

func (b *CappedBuffer) Write(p []byte) (int, error) {
	if remain := b.Max - int64(b.Buf.Len()); remain > 0 {
		if int64(len(p)) > remain {
			b.Buf.Write(p[:remain])
		} else {
			b.Buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *CappedBuffer) String() string { return b.Buf.String() }

// Full reports whether the buffer has reached its cap.
func (b *CappedBuffer) Full() bool { return int64(b.Buf.Len()) >= b.Max }

// DirEntry describes one entry returned by Sandbox.ListDir.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// ErrNoWorkDir is returned by ReadFile, WriteFile and ListDir when the sandbox
// has no persistent working directory.
var ErrNoWorkDir = errors.New("sandbox: no persistent working directory configured")

// ExecStreamer is optionally implemented by Sandbox backends that support
// streaming command output. Output is written to stdout/stderr as it arrives;
// the returned ExecResult contains ExitCode and TimedOut but its Stdout and
// Stderr fields are empty.
type ExecStreamer interface {
	ExecStream(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (*ExecResult, error)
}
