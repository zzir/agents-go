package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/zzir/agents-go/sandbox"
)

var _ sandbox.TerminalOpener = (*Sandbox)(nil)

// OpenTerminal implements sandbox.TerminalOpener: it opens a new SSH session
// on the existing client connection, requests a PTY and starts an interactive
// shell in the sandbox working directory. Multiple terminals can be open at
// once; each uses its own session.
func (s *Sandbox) OpenTerminal(ctx context.Context, opts sandbox.TerminalOptions) (sandbox.Terminal, error) {
	// Session setup below is a fixed short sequence of blocking calls with no
	// context plumbing in x/crypto/ssh; honor an already-cancelled ctx up front.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := s.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh sandbox: new session: %w", err)
	}
	// Empty TerminalModes: the remote side keeps its defaults (echo on, canonical
	// mode off handled by the shell); xterm.js expects exactly that.
	if err := session.RequestPty(opts.EffectiveTerm(), opts.EffectiveRows(), opts.EffectiveCols(), ssh.TerminalModes{}); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("ssh sandbox: request pty: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("ssh sandbox: stdin pipe: %w", err)
	}
	// With a PTY the remote fd 2 is the PTY slave too, so stderr arrives merged
	// into the stdout stream; no separate stderr pipe is needed.
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("ssh sandbox: stdout pipe: %w", err)
	}
	if err := session.Start(buildShellCommand(s.opts.WorkDir, opts.Env, opts.Shell)); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("ssh sandbox: start shell: %w", err)
	}
	return &terminal{session: session, stdin: stdin, stdout: stdout}, nil
}

// buildShellCommand assembles the remote command line that starts the
// interactive shell: cd into dir when set, apply env overrides (sshd rejects
// Setenv for names outside AcceptEnv, so env must ride the command line, as
// buildCommand does for Exec), then exec the shell. An empty shell falls back
// to the user's login shell — sshd exports SHELL — with /bin/sh as a last
// resort; it is expanded remotely, hence unquoted.
func buildShellCommand(dir string, env map[string]string, shell []string) string {
	var b strings.Builder
	if dir != "" {
		b.WriteString("cd ")
		b.WriteString(sandbox.ShellQuote(dir))
		b.WriteString(" && ")
	}
	b.WriteString("exec ")
	if len(env) > 0 {
		b.WriteString("env")
		for _, k := range slices.Sorted(maps.Keys(env)) {
			b.WriteByte(' ')
			b.WriteString(sandbox.ShellQuote(k + "=" + env[k]))
		}
		b.WriteByte(' ')
	}
	if len(shell) == 0 {
		b.WriteString(`"${SHELL:-/bin/sh}" -l`)
		return b.String()
	}
	for i, c := range shell {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(sandbox.ShellQuote(c))
	}
	return b.String()
}

// terminal is one interactive SSH session with a PTY.
type terminal struct {
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader

	waitOnce sync.Once
	waitCode int
	waitErr  error
}

func (t *terminal) Read(p []byte) (int, error)  { return t.stdout.Read(p) }
func (t *terminal) Write(p []byte) (int, error) { return t.stdin.Write(p) }

// Resize adjusts the remote PTY. ssh.Session.WindowChange takes (height, width).
func (t *terminal) Resize(cols, rows int) error {
	return t.session.WindowChange(rows, cols)
}

// Close tears down the SSH channel. Whether the remote shell dies with it
// depends on the sshd implementation — the same caveat as Exec timeouts; with
// a PTY attached, virtually all servers send SIGHUP to the foreground group.
func (t *terminal) Close() error {
	err := t.session.Close()
	if err == nil || errors.Is(err, io.EOF) {
		// io.EOF here means the session was already closed.
		return nil
	}
	return err
}

// Wait blocks until the shell exits and reports its exit code; -1 when the
// session ended without delivering one (signal kill, transport closed first).
func (t *terminal) Wait() (int, error) {
	t.waitOnce.Do(func() {
		werr := t.session.Wait()
		var exitErr *ssh.ExitError
		var missingErr *ssh.ExitMissingError
		switch {
		case werr == nil:
			t.waitCode = 0
		case errors.As(werr, &exitErr):
			t.waitCode = exitErr.ExitStatus()
		case errors.As(werr, &missingErr):
			t.waitCode = -1
		default:
			t.waitCode, t.waitErr = -1, werr
		}
	})
	return t.waitCode, t.waitErr
}
