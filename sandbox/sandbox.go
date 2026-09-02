// Package sandbox runs untrusted, agent-generated code in an isolated
// environment and exposes it to an agent as a tool. The Sandbox interface is
// backend-agnostic: LocalSandbox lives here, the Docker backend in the
// sandbox/docker module and the E2B backend in sandbox/e2b.
//
// A sandbox executes a command in a working directory after writing the request
// files into it. Backends enforce isolation (no network, read-only root,
// dropped capabilities) and a per-command time limit by default; memory and CPU
// limits apply when the caller sets Options.Limits.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// DefaultTimeout bounds an execution when ExecRequest.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// DefaultMaxOutputBytes caps each captured output stream when
// ExecRequest.MaxOutputBytes is zero.
const DefaultMaxOutputBytes int64 = 1 << 20 // 1 MiB

// DefaultMaxReadFileBytes caps ReadFile when a backend's MaxReadFileBytes
// option is zero, so one read_file call cannot load an arbitrarily large file
// into host memory.
const DefaultMaxReadFileBytes int64 = 8 << 20 // 8 MiB

// ErrReadLimitExceeded is returned (wrapped) by ReadFile when the file is
// larger than the backend's read limit — an error, never a silent truncation.
var ErrReadLimitExceeded = errors.New("file exceeds read limit")

// ReadAllLimited reads r to completion but fails with ErrReadLimitExceeded
// when the content exceeds limit bytes; limit <= 0 means
// DefaultMaxReadFileBytes. At most limit+1 bytes are read.
func ReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxReadFileBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("sandbox: %w (%d bytes)", ErrReadLimitExceeded, limit)
	}
	return data, nil
}

// ShellQuote returns s as a single POSIX shell token: wrapped in single quotes,
// with every embedded single quote closed, escaped and reopened:
//
//	don't  ->  'don'\''t'
//
// A backend that assembles an "sh -c" command line must pass every interpolated
// value — path, argument, environment entry — through it, so nothing in the
// value can be read as shell syntax.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteAndClose writes content to f and closes it, reporting the first
// failure. It is the write half of an O_EXCL create for backends on an OS
// filesystem; the caller removes the file on error.
func WriteAndClose(f *os.File, content []byte) error {
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

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
	// CreateExclusive atomically creates path with content (creating parent
	// directories) and fails with fs.ErrExist if it already exists; it leaves
	// no partial file behind on failure. apply_patch's Add/Move rely on it.
	CreateExclusive(ctx context.Context, path string, content []byte) error
	// ListDir lists entries in a sandbox directory (empty path = working dir),
	// in no particular order.
	ListDir(ctx context.Context, path string) ([]DirEntry, error)
	// RemoveFile removes a file in the sandbox's persistent working directory.
	RemoveFile(ctx context.Context, path string) error
	// Rename moves a file within the sandbox's persistent working directory,
	// creating the destination's parent directories. apply_patch parks a file
	// too large to snapshot with it (spec §2.7s).
	Rename(ctx context.Context, oldPath, newPath string) error
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
	// Env sets environment variables for the process, over the sandbox's own.
	// The host environment is never passed through (see LocalOptions.InheritHostEnv).
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

// MergeEnv returns base overlaid by override — the environment a command runs
// with: the sandbox's own variables, each overridden by the request's. A fresh
// map, so neither input is mutated.
func MergeEnv(base, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
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
	buf bytes.Buffer
	Max int64
}

func (b *CappedBuffer) Write(p []byte) (int, error) {
	if remain := b.Max - int64(b.buf.Len()); remain > 0 {
		if int64(len(p)) > remain {
			b.buf.Write(p[:remain])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *CappedBuffer) String() string { return b.buf.String() }

// Full reports whether the buffer has reached its cap.
func (b *CappedBuffer) Full() bool { return int64(b.buf.Len()) >= b.Max }

// DirEntry describes one entry returned by Sandbox.ListDir.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// ErrNoWorkDir is returned by the file operations — ReadFile, WriteFile,
// CreateExclusive, ListDir, RemoveFile and Rename — when the sandbox has no
// persistent working directory.
var ErrNoWorkDir = errors.New("sandbox: no persistent working directory configured")

// ErrOutsideWorkDir is returned (wrapped) by file operations that refuse a
// path outside the working directory. Only docker's bind-mount mode raises it:
// its file operations run on the host side of the mount (decisions §5.14).
var ErrOutsideWorkDir = errors.New("sandbox: path outside the working directory")

// ExecStreamer is optionally implemented by Sandbox backends that support
// streaming command output. Output is written to stdout/stderr as it arrives;
// the returned ExecResult contains ExitCode and TimedOut but its Stdout and
// Stderr fields are empty.
type ExecStreamer interface {
	ExecStream(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (*ExecResult, error)
}
