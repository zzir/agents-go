package sandbox

import (
	"context"
	"errors"
	"io"
)

// DefaultTerminalTerm is the TERM value used when TerminalOptions.Term is empty.
const DefaultTerminalTerm = "xterm-256color"

// Default PTY dimensions used when TerminalOptions leaves Cols/Rows zero.
const (
	DefaultTerminalCols = 80
	DefaultTerminalRows = 24
)

// ErrTerminalUnsupported is returned (wrapped) by TerminalOpener.OpenTerminal
// when the backend cannot provide an interactive terminal in its current
// configuration (e.g. the docker backend outside Persistent mode).
var ErrTerminalUnsupported = errors.New("sandbox: interactive terminal not supported")

// TerminalOptions configures an interactive terminal session.
type TerminalOptions struct {
	// Cols and Rows set the initial PTY size. Zero values default to
	// DefaultTerminalCols x DefaultTerminalRows.
	Cols, Rows int
	// Term is the TERM environment variable value seen by the shell; empty
	// means DefaultTerminalTerm.
	Term string
	// Shell overrides the command started in the PTY. Empty selects the
	// backend default: the remote login shell for SSH; for docker, bash when
	// the image ships it, otherwise /bin/sh.
	Shell []string
	// Env sets additional environment variables for the shell.
	Env map[string]string
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

// EffectiveTerm returns the configured TERM value or DefaultTerminalTerm.
func (o TerminalOptions) EffectiveTerm() string {
	if o.Term == "" {
		return DefaultTerminalTerm
	}
	return o.Term
}

// Terminal is a live interactive shell session inside a sandbox. Read returns
// terminal output (including ANSI escape sequences) and reports io.EOF when
// the shell exits; Write sends raw user input. Both directions carry the raw
// byte stream as seen by the PTY.
//
// Close terminates the session and releases transport resources; it is safe
// to call more than once and concurrently with Read/Write.
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
// interactive terminals, mirroring how ExecStreamer extends Exec. The context
// bounds session establishment only; the returned Terminal lives until Close.
type TerminalOpener interface {
	OpenTerminal(ctx context.Context, opts TerminalOptions) (Terminal, error)
}
