package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ShellSession is a shell that stays alive between commands, so `cd`, exported
// variables, an activated virtualenv and a started background process all
// survive into the next call.
//
// Each Exec in a fresh shell is stateless, which reads as a bug to a model:
// it runs `cd build`, then `make`, and make runs in the wrong directory. The
// alternative it reaches for on its own — chaining everything into one enormous
// `&&` line — is worse to read, worse to fail, and loses the output boundaries.
//
// Completion is detected with a SENTINEL: after each command the session prints
// a token and the exit status, and output is read until that token appears.
// There is no other reliable signal on a PTY — a prompt is configurable, silence
// means nothing, and a command that prints nothing is indistinguishable from one
// still running.
type ShellSession struct {
	term Terminal
	// sentinel is what appears in the OUTPUT; head and tail are the two halves
	// the command line carries. See newSentinel.
	sentinel   string
	head, tail string

	// chunks carries what the reader goroutine has read. A background reader
	// is what makes the timeout real: Terminal is an io.ReadWriteCloser with no
	// deadline, so a Read on the calling goroutine blocks forever on a command
	// that never finishes and no timer can interrupt it.
	chunks  chan []byte
	readErr chan error
	// done releases the reader when the session closes with chunks full: a
	// channel send is not unblocked by closing the Terminal, so a timed-out
	// flooding command would otherwise pin the goroutine (and its buffered
	// output) for the life of the process.
	done chan struct{}

	mu sync.Mutex
	// lastLine is what was written for the command in flight, so its echo can
	// be stripped from the output exactly rather than guessed at.
	lastLine string
	buf      []byte
	closed   bool
}

// OpenShellSession starts a persistent shell in the sandbox.
//
// The backend must support interactive terminals: a session is a shell held
// open, which Exec cannot express.
func OpenShellSession(ctx context.Context, sb Sandbox, opts TerminalOptions) (*ShellSession, error) {
	opener, ok := sb.(TerminalOpener)
	if !ok {
		return nil, fmt.Errorf("sandbox: %T does not support shell sessions", sb)
	}
	term, err := opener.OpenTerminal(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("sandbox: opening shell session: %w", err)
	}
	s := newShellSession(term)

	// Drain whatever the shell says on startup — a banner, a prompt — so the
	// first command's output does not begin with someone else's text.
	if err := s.settle(ctx); err != nil {
		_ = term.Close()
		return nil, err
	}
	return s, nil
}

// newShellSession builds a session with a fresh sentinel and starts its reader.
func newShellSession(term Terminal) *ShellSession {
	head, tail := newSentinel()
	s := &ShellSession{
		term:     term,
		sentinel: head + tail,
		head:     head,
		tail:     tail,
		chunks:   make(chan []byte, 16),
		readErr:  make(chan error, 1),
		done:     make(chan struct{}),
	}
	go s.readLoop()
	return s
}

// readLoop feeds chunks until the terminal ends. It exits on Close, because
// Close closes the terminal and the blocked Read returns.
func (s *ShellSession) readLoop() {
	defer close(s.chunks)
	buf := make([]byte, 4096)
	for {
		n, err := s.term.Read(buf)
		if n > 0 {
			select {
			case s.chunks <- append([]byte(nil), buf[:n]...):
			case <-s.done:
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.readErr <- err
			}
			return
		}
	}
}

// newSentinel mints a token for one session, in two halves.
//
// Random and per session, because a fixed token is one a command can print:
// `echo __DONE__` would end the read early and hand the model a truncated
// result with a garbage exit code.
//
// Two halves, because a PTY echoes what is written to it. A command line
// carrying the whole token would come back in the output, and that echo is
// indistinguishable from the real thing — the read would stop at the echo, one
// command early, forever after. The command line carries the halves as separate
// printf arguments; only the OUTPUT ever contains them joined.
func newSentinel() (head, tail string) {
	var b [12]byte
	// As of Go 1.24 crypto/rand.Read never fails; it aborts the program if the
	// OS source is unavailable.
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return "__agents_sh_" + h[:12], h[12:] + "__"
}

// settle runs one no-op command so the session is at a known state, discarding
// anything the shell printed before it.
func (s *ShellSession) settle(ctx context.Context) error {
	_, _, err := s.Run(ctx, ":", 10*time.Second)
	return err
}

// Run executes one command and returns its output and exit status.
//
// The command is sent verbatim, then the sentinel echo. Splitting them with a
// newline rather than `;` means a command ending in a comment, a heredoc or a
// trailing backslash cannot swallow the sentinel and hang the read forever.
func (s *ShellSession) Run(ctx context.Context, cmd string, timeout time.Duration) (output string, exitCode int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", -1, errors.New("sandbox: shell session is closed")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// The halves are separate arguments, so the echoed line never contains the
	// token the reader is looking for. The newline before printf (rather than
	// `;`) means a command ending in a comment, a heredoc or a trailing
	// backslash cannot swallow it and hang the read forever.
	line := cmd + "\nprintf '%s%s %d\\n' '" + s.head + "' '" + s.tail + "' $?\n"
	s.lastLine = line
	if _, err := io.WriteString(s.term, line); err != nil {
		return "", -1, fmt.Errorf("sandbox: writing to shell session: %w", err)
	}

	out, code, err := s.readUntilSentinel(ctx, timeout)
	if err != nil {
		// The session is no longer at a known state: the command may still be
		// running and its output will arrive in the middle of the next one.
		// Closing is the honest outcome — a session that silently interleaves
		// two commands' output is worse than one that is gone.
		_ = s.closeLocked()
		return out, -1, err
	}
	return out, code, nil
}

// readUntilSentinel reads until the token appears, returning what came before
// it and the exit status that follows it.
func (s *ShellSession) readUntilSentinel(ctx context.Context, timeout time.Duration) (string, int, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if idx := bytes.Index(s.buf, []byte(s.sentinel)); idx >= 0 {
			// The exit status follows the token on the same line.
			rest := s.buf[idx+len(s.sentinel):]
			if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
				out := string(s.buf[:idx])
				code, _ := strconv.Atoi(strings.TrimSpace(string(rest[:nl])))
				s.buf = append([]byte(nil), rest[nl+1:]...)
				return trimEcho(out, s.lastLine), code, nil
			}
			// The status line is not complete yet; keep reading.
		}
		select {
		case chunk, ok := <-s.chunks:
			if !ok {
				return string(s.buf), -1, errors.New("sandbox: shell session ended")
			}
			s.buf = append(s.buf, chunk...)
		case err := <-s.readErr:
			return string(s.buf), -1, fmt.Errorf("sandbox: reading from shell session: %w", err)
		case <-timer.C:
			return string(s.buf), -1, fmt.Errorf("sandbox: shell session command timed out after %s", timeout)
		case <-ctx.Done():
			return string(s.buf), -1, ctx.Err()
		}
	}
}

// trimEcho removes the shell's echo of what was written.
//
// A PTY echoes its input, so the command AND the printf that carries the
// sentinel come back as output — reported to the model as part of the result
// unless dropped. The echo is a prefix and is stripped as one, line by line
// against what was actually written, rather than by pattern: a heuristic would
// also eat a command that legitimately printed its own text back.
//
// A terminal with echo disabled emits nothing to strip, and the loop simply
// finds no matches.
func trimEcho(out, written string) string {
	outLines := strings.Split(out, "\n")
	for _, echoed := range strings.Split(strings.TrimRight(written, "\n"), "\n") {
		if len(outLines) == 0 {
			break
		}
		if strings.TrimRight(outLines[0], "\r") != echoed {
			break
		}
		outLines = outLines[1:]
	}
	return strings.TrimLeft(strings.Join(outLines, "\n"), "\r\n")
}

// Close ends the session.
func (s *ShellSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *ShellSession) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.done)
	return s.term.Close()
}

// sessionPool holds the named shells one exec_command tool has open.
//
// It belongs to the tool rather than the sandbox because the names are the
// model's: two agents sharing a sandbox should not collide on "build", and a
// pool per tool gives each its own namespace for free.
type sessionPool struct {
	mu       sync.Mutex
	sessions map[string]*ShellSession
}

func newSessionPool() *sessionPool {
	return &sessionPool{sessions: map[string]*ShellSession{}}
}

// run executes cmd in the named session, opening it on first use.
//
// A session that failed is dropped rather than reused: a timed-out shell may
// still be running the previous command, and its output would arrive in the
// middle of the next one. Reopening costs a shell startup; getting two
// commands' output interleaved costs the model's trust in every result.
func (p *sessionPool) run(ctx context.Context, sb Sandbox, name, cmd string, timeout time.Duration) (string, int, error) {
	p.mu.Lock()
	s, ok := p.sessions[name]
	if !ok {
		var err error
		s, err = OpenShellSession(ctx, sb, TerminalOptions{})
		if err != nil {
			p.mu.Unlock()
			return "", -1, err
		}
		p.sessions[name] = s
	}
	p.mu.Unlock()

	out, code, err := s.Run(ctx, cmd, timeout)
	if err != nil {
		p.mu.Lock()
		if p.sessions[name] == s {
			delete(p.sessions, name)
		}
		p.mu.Unlock()
		_ = s.Close()
	}
	return out, code, err
}

// Close ends every session in the pool.
func (p *sessionPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	for name, s := range p.sessions {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(p.sessions, name)
	}
	return errors.Join(errs...)
}

// formatSessionResult renders a session command's outcome the way formatResult
// renders a one-shot one: a session has a single interleaved stream rather than
// separate stdout and stderr, because that is what a PTY gives.
func formatSessionResult(out string, code, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", code)
	if trimmed := strings.TrimRight(out, "\n"); trimmed != "" {
		b.WriteString("output:\n")
		b.WriteString(truncateWithInfo(trimmed, limit))
		b.WriteString("\n")
	}
	return b.String()
}
