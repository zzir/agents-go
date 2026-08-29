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

// sessionBufCap bounds the output retained while waiting for the sentinel; a
// flooding command drops its middle, keeping the head and the live tail.
const sessionBufCap = int(DefaultMaxOutputBytes)

// readUntilSentinel reads until the token appears, returning what came before
// it and the exit status that follows it.
func (s *ShellSession) readUntilSentinel(ctx context.Context, timeout time.Duration) (string, int, error) {
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
				return string(s.buf), -1, errors.New("sandbox: shell session ended")
			}
			s.buf = append(s.buf, chunk...)
			scanFrom = s.capBuf(scanFrom)
		case err := <-s.readErr:
			return string(s.buf), -1, fmt.Errorf("sandbox: reading from shell session: %w", err)
		case <-timer.C:
			return string(s.buf), -1, fmt.Errorf("sandbox: shell session command timed out after %s", timeout)
		case <-ctx.Done():
			return string(s.buf), -1, ctx.Err()
		}
	}
}

// capBuf drops the middle of s.buf once it doubles sessionBufCap, keeping the
// head and the tail the sentinel arrives in, and returns scanFrom remapped.
// scanFrom trails the buffer's end, so the cut (and the seam it creates) stays
// below the scan window and can never be misread as a sentinel.
func (s *ShellSession) capBuf(scanFrom int) int {
	headKeep := sessionBufCap * 3 / 5 // truncateWithInfo's head/tail split
	cut := len(s.buf) - (sessionBufCap - headKeep)
	if len(s.buf) <= 2*sessionBufCap || cut > scanFrom {
		return scanFrom
	}
	s.buf = append(s.buf[:headKeep], s.buf[cut:]...)
	return scanFrom - (cut - headKeep)
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
//
// The namespace is per TOOL, not per run: a *Tool built once and used by
// several concurrent runs gives all of them the same pool, so two runs of the
// same agent that both open "build" get the SAME shell — their commands
// interleave in one PTY, and one run's `cd` moves the other. A host that runs
// an agent concurrently and wants isolation builds the tool per run.
type sessionPool struct {
	mu sync.Mutex
	// closed is the pool's terminal state. It exists because the open runs
	// outside the lock (see session): without it, a shell that landed after
	// Close emptied the map would be held by a pool nobody closes again — the
	// leaked PTY (a remote ssh session on that backend) that
	// CodeToolConfig.RegisterCloser exists to prevent.
	closed   bool
	sessions map[string]*ShellSession
}

// errSessionPoolClosed is what a named command gets once the pool's owner has
// released its shells: the sandbox is being torn down, so opening a fresh shell
// on it would only leak one.
var errSessionPoolClosed = errors.New("sandbox: session pool is closed")

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

// session returns the named session, opening one on first use.
//
// The open runs OUTSIDE p.mu: on the ssh backend it is a network round-trip
// plus a PTY handshake and a settle command, and holding the lock across it
// stalls every OTHER name's command for that whole time. Under the lock there
// is only a map lookup and a map write. Two callers racing the same NEW name
// therefore both open a shell; the loser closes its own and takes the winner's,
// so the pool still holds exactly one session per name.
//
// The same window lets Close run while a shell is being opened, which is why
// the second critical section re-checks p.closed: Close only reaches the
// sessions the map holds, so one landing after it has to close itself.
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

// Close ends every session in the pool, and is final: a command that arrives
// afterwards fails rather than opening a shell on a sandbox whose owner has
// already let go of it.
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
