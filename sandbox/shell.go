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

// ShellSession is a shell that stays alive between commands; completion is
// detected with a sentinel printed after each command (spec §2.7k).
type ShellSession struct {
	term Terminal
	// sentinel is what appears in the OUTPUT; head and tail are the two halves
	// the command line carries. See newSentinel.
	sentinel   string
	head, tail string

	// chunks carries what the reader goroutine has read; Terminal has no read
	// deadline, so only a background reader makes the timeout real.
	chunks  chan []byte
	readErr chan error
	// done releases a reader blocked on a full chunks channel when the session
	// closes; closing the Terminal does not unblock a channel send.
	done chan struct{}

	// closeOnce closes the terminal at most once: Close preempting a command
	// in flight can race that command's own error-path close.
	closeOnce sync.Once
	closeErr  error

	mu sync.Mutex
	// lastLine is what was written for the command in flight, so its echo can
	// be stripped from the output exactly rather than guessed at.
	lastLine string
	buf      []byte
	closed   bool
}

// OpenShellSession starts a persistent shell in the sandbox, which must
// implement TerminalOpener.
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

// newSentinel mints a random per-session token in two halves, so the PTY's
// echo never contains the joined token (spec §2.7k).
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
func (s *ShellSession) Run(ctx context.Context, cmd string, timeout time.Duration) (output string, exitCode int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", -1, errors.New("sandbox: shell session is closed")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// A newline before printf (not `;`): a command ending in a comment, a
	// heredoc or a trailing backslash cannot swallow the sentinel.
	line := cmd + "\nprintf '%s%s %d\\n' '" + s.head + "' '" + s.tail + "' $?\n"
	s.lastLine = line
	if _, err := io.WriteString(s.term, line); err != nil {
		return "", -1, fmt.Errorf("sandbox: writing to shell session: %w", err)
	}

	out, code, err := s.readUntilSentinel(ctx, timeout)
	if err != nil {
		// The session is no longer at a known state; close rather than
		// interleave the next command's output with this one's (spec §2.7k).
		_ = s.closeLocked()
		return out, -1, err
	}
	return out, code, nil
}

// sessionBufCap bounds the output retained while waiting for the sentinel; a
// flooding command drops its middle, keeping the head and the live tail.
const sessionBufCap = int(DefaultMaxOutputBytes)

// readUntilSentinel reads until the token appears, returning the output before
// it and the exit status after it; every failure carries the partial output.
func (s *ShellSession) readUntilSentinel(ctx context.Context, timeout time.Duration) (string, int, error) {
	partial := func() string { return trimEcho(string(s.buf), s.lastLine) }
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	scanFrom := 0 // settled bytes are never rescanned; new chunks extend the window
	for {
		if idx := bytes.Index(s.buf[scanFrom:], []byte(s.sentinel)); idx >= 0 {
			idx += scanFrom
			// The exit status follows the token on the same line.
			rest := s.buf[idx+len(s.sentinel):]
			if before, after, ok := bytes.Cut(rest, []byte{'\n'}); ok {
				out := string(s.buf[:idx])
				code, _ := strconv.Atoi(strings.TrimSpace(string(before)))
				s.buf = append([]byte(nil), after...)
				return trimEcho(out, s.lastLine), code, nil
			}
			scanFrom = idx // status line incomplete; keep the token in the window
		} else {
			scanFrom = max(0, len(s.buf)-len(s.sentinel)+1)
		}
		select {
		case chunk, ok := <-s.chunks:
			if !ok {
				return partial(), -1, errors.New("sandbox: shell session ended")
			}
			s.buf = append(s.buf, chunk...)
			scanFrom = s.capBuf(scanFrom)
		case err := <-s.readErr:
			return partial(), -1, fmt.Errorf("sandbox: reading from shell session: %w", err)
		case <-timer.C:
			return partial(), -1, fmt.Errorf("sandbox: shell session command timed out after %s", timeout)
		case <-ctx.Done():
			return partial(), -1, ctx.Err()
		}
	}
}

// capBuf drops the middle of s.buf once it doubles sessionBufCap and returns
// scanFrom remapped; the cut stays below the scan window (spec §2.7k).
func (s *ShellSession) capBuf(scanFrom int) int {
	headKeep := sessionBufCap * 3 / 5 // truncateWithInfo's head/tail split
	cut := len(s.buf) - (sessionBufCap - headKeep)
	if len(s.buf) <= 2*sessionBufCap || cut > scanFrom {
		return scanFrom
	}
	s.buf = append(s.buf[:headKeep], s.buf[cut:]...)
	return scanFrom - (cut - headKeep)
}

// trimEcho removes the PTY's echo of what was written as an exact line-by-line
// prefix (spec §2.7k); with echo disabled the loop finds no match.
func trimEcho(out, written string) string {
	outLines := strings.Split(out, "\n")
	for echoed := range strings.SplitSeq(strings.TrimRight(written, "\n"), "\n") {
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

// Close ends the session. The terminal closes BEFORE s.mu is taken, so a
// command in flight is preempted rather than waited out (spec §2.7k).
func (s *ShellSession) Close() error {
	err := s.closeTerm()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return err
}

func (s *ShellSession) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.closeTerm()
}

// closeTerm closes the terminal once, releasing the reader goroutine and with
// it any Run blocked on the sentinel.
func (s *ShellSession) closeTerm() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.closeErr = s.term.Close()
	})
	return s.closeErr
}

// sessionPool holds the named shells one exec_command tool has open. The
// namespace is per TOOL, not per run (spec §2.7k).
type sessionPool struct {
	mu sync.Mutex
	// closed is final: the open runs outside the lock (see session), and a
	// shell landing after Close would otherwise be held by nobody.
	closed   bool
	sessions map[string]*ShellSession
}

// errSessionPoolClosed is what a named command gets once the pool's owner has
// released its shells (spec §2.7k).
var errSessionPoolClosed = errors.New("sandbox: session pool is closed")

func newSessionPool() *sessionPool {
	return &sessionPool{sessions: map[string]*ShellSession{}}
}

// run executes cmd in the named session, opening it on first use. A session
// that failed is dropped rather than reused (spec §2.7k).
func (p *sessionPool) run(ctx context.Context, sb Sandbox, name, cmd string, timeout time.Duration) (string, int, error) {
	s, err := p.session(ctx, sb, name)
	if err != nil {
		return "", -1, err
	}

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

// session returns the named session, opening one on first use OUTSIDE p.mu;
// the loser of a race takes the winner's (spec §2.7k).
func (p *sessionPool) session(ctx context.Context, sb Sandbox, name string) (*ShellSession, error) {
	p.mu.Lock()
	s, ok := p.sessions[name]
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, errSessionPoolClosed
	}
	if ok {
		return s, nil
	}

	opened, err := OpenShellSession(ctx, sb, TerminalOptions{})
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = opened.Close()
		return nil, errSessionPoolClosed
	}
	if existing, ok := p.sessions[name]; ok {
		p.mu.Unlock()
		_ = opened.Close()
		return existing, nil
	}
	p.sessions[name] = opened
	p.mu.Unlock()
	return opened, nil
}

// Close ends every session and is final: a later command fails rather than
// opening a shell on a released sandbox (spec §2.7k).
func (p *sessionPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	var errs []error
	for name, s := range p.sessions {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(p.sessions, name)
	}
	return errors.Join(errs...)
}

// formatSessionResult renders a session command's outcome like formatResult;
// a PTY gives one interleaved stream, not separate stdout and stderr.
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
