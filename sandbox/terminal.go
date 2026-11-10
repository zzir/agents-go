package sandbox

import (
	"context"
	"errors"
	"io"
)

// DefaultTerminalTerm is the TERM value every backend's terminal shell sees.
const DefaultTerminalTerm = "xterm-256color"

// Default PTY dimensions used when TerminalOptions leaves Cols/Rows zero.
const (
	DefaultTerminalCols = 80
	DefaultTerminalRows = 24
)

// ErrTerminalUnsupported is returned (wrapped) by TerminalOpener.OpenTerminal
// when the backend's current configuration has no interactive terminal.
var ErrTerminalUnsupported = errors.New("sandbox: interactive terminal not supported")

// TerminalOptions configures an interactive terminal session.
type TerminalOptions struct {
	// Cols and Rows set the initial PTY size. Zero values default to
	// DefaultTerminalCols x DefaultTerminalRows.
	Cols, Rows int
}

// EffectiveCols returns the configured column count or DefaultTerminalCols.
func (o TerminalOptions) EffectiveCols() int {
	if o.Cols <= 0 {
		return DefaultTerminalCols
	}
	return o.Cols
}

// EffectiveRows returns the configured row count or DefaultTerminalRows.
func (o TerminalOptions) EffectiveRows() int {
	if o.Rows <= 0 {
		return DefaultTerminalRows
	}
	return o.Rows
}

// Terminal is a live interactive shell session inside a sandbox: Read returns
// the raw PTY output (ANSI escapes included) and io.EOF when the shell exits,
// Write sends raw user input. Close terminates the session; it is safe to
// call more than once and concurrently with Read/Write.
type Terminal interface {
	io.ReadWriteCloser
	// Resize changes the PTY size.
	Resize(cols, rows int) error
	// Wait blocks until the shell has exited and returns its exit code, or -1
	// when the code is unknown (e.g. the transport closed first). Call it
	// after Read has returned io.EOF; backends may resolve the code lazily.
	Wait() (int, error)
}

// TerminalOpener is optionally implemented by Sandbox backends that support
// interactive terminals. The context bounds session establishment only; the
// returned Terminal lives until Close.
type TerminalOpener interface {
	OpenTerminal(ctx context.Context, opts TerminalOptions) (Terminal, error)
}
