package sandbox

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTerminal is a shell stand-in: it echoes what is written (as a PTY does)
// and runs a scripted responder over the command lines it sees.
type fakeTerminal struct {
	mu     sync.Mutex
	out    []byte
	closed bool
	ready  chan struct{}
	// respond produces the output for one command line; the harness appends
	// the sentinel line itself.
	respond func(cmd string) (string, int)
	// noSentinel suppresses the sentinel line: the command never finishes.
	noSentinel bool
	partial    string
	written    string
	lastCode   int
}

func newFakeTerminal(respond func(string) (string, int)) *fakeTerminal {
	return &fakeTerminal{ready: make(chan struct{}, 64), respond: respond}
}

func (f *fakeTerminal) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	// A PTY echoes input back.
	f.out = append(f.out, p...)
	f.written += string(p)
	f.partial += string(p)
	for {
		line, rest, ok := strings.Cut(f.partial, "\n")
		if !ok {
			break
		}
		f.partial = rest
		if f.respond == nil {
			// A terminal that never answers: the command never finishes.
			continue
		}
		if strings.HasPrefix(line, "printf ") {
			// The sentinel line: emit the token and the status of the command
			// before it. The harness tracks that in lastCode.
			if !f.noSentinel {
				f.out = append(f.out, []byte(f.sentinelLine(line))...)
			}
			continue
		}
		body, code := f.respond(line)
		f.lastCode = code
		if body != "" {
			f.out = append(f.out, []byte(body)...)
		}
	}
	select {
	case f.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}

// sentinelLine emulates the shell running the printf: it JOINS the two quoted
// halves, which is what makes the echoed line and the printed one distinct.
func (f *fakeTerminal) sentinelLine(printfLine string) string {
	var parts []string
	for field := range strings.FieldsSeq(printfLine) {
		if strings.HasPrefix(field, "'__agents_sh_") || (strings.HasPrefix(field, "'") && strings.HasSuffix(field, "__'")) {
			parts = append(parts, strings.Trim(field, "'"))
		}
	}
	return strings.Join(parts, "") + " " + itoa(f.lastCode) + "\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func (f *fakeTerminal) Read(p []byte) (int, error) {
	for {
		f.mu.Lock()
		if len(f.out) > 0 {
			n := copy(p, f.out)
			f.out = f.out[n:]
			f.mu.Unlock()
			return n, nil
		}
		closed := f.closed
		f.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		select {
		case <-f.ready:
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	select {
	case f.ready <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeTerminal) Resize(int, int) error { return nil }
func (f *fakeTerminal) Wait() (int, error)    { return 0, nil }

func newSession(t *testing.T, respond func(string) (string, int)) (*ShellSession, *fakeTerminal) {
	t.Helper()
	term := newFakeTerminal(respond)
	s := newShellSession(term)
	if err := s.settle(context.Background()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	return s, term
}

// The point of a session: `cd build` then `make` must run in build.
func TestShellSession_StateSurvivesBetweenCommands(t *testing.T) {
	cwd := "/"
	s, _ := newSession(t, func(cmd string) (string, int) {
		switch {
		case strings.HasPrefix(cmd, "cd "):
			cwd = strings.TrimSpace(strings.TrimPrefix(cmd, "cd "))
			return "", 0
		case cmd == "pwd":
			return cwd + "\n", 0
		}
		return "", 0
	})
	defer s.Close()

	ctx := context.Background()
	if _, code, err := s.Run(ctx, "cd build", time.Second); err != nil || code != 0 {
		t.Fatalf("cd: code=%d err=%v", code, err)
	}
	out, code, err := s.Run(ctx, "pwd", time.Second)
	if err != nil || code != 0 {
		t.Fatalf("pwd: code=%d err=%v", code, err)
	}
	if strings.TrimSpace(out) != "build" {
		t.Errorf("pwd = %q, want the directory the previous command changed to", out)
	}
}

func TestShellSession_ReportsExitStatus(t *testing.T) {
	s, _ := newSession(t, func(cmd string) (string, int) {
		if cmd == "false" {
			return "", 1
		}
		return "ok\n", 0
	})
	defer s.Close()

	if _, code, _ := s.Run(context.Background(), "false", time.Second); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	out, code, _ := s.Run(context.Background(), "true", time.Second)
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Errorf("out=%q code=%d", out, code)
	}
}

// A fixed token could be printed BY a command. `echo __DONE__` would end the
// read early and hand the model a truncated result with a garbage status.
func TestShellSession_SentinelIsPerSession(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		head, tail := newSentinel()
		if seen[head+tail] {
			t.Fatal("two sessions minted the same sentinel")
		}
		seen[head+tail] = true
	}
}

// A PTY echoes the command line. If it carried the whole token, the echo would
// be indistinguishable from the printed one and every read would stop a command
// early, forever after.
func TestShellSession_EchoedLineCannotContainTheSentinel(t *testing.T) {
	s, term := newSession(t, func(string) (string, int) { return "hi\n", 0 })
	defer s.Close()
	if _, _, err := s.Run(context.Background(), "echo hi", time.Second); err != nil {
		t.Fatal(err)
	}
	term.mu.Lock()
	defer term.mu.Unlock()
	// The echo carries the halves separately; only output joins them.
	if !strings.Contains(term.written, s.head) || !strings.Contains(term.written, s.tail) {
		t.Fatalf("the command line lost the sentinel halves:\n%s", term.written)
	}
	if strings.Contains(term.written, s.sentinel) {
		t.Errorf("the echoed command line contains the whole sentinel:\n%s", term.written)
	}
}

// The PTY echoes the printf that carries the sentinel; it must not be reported
// as part of the command's output.
func TestShellSession_StripsTheSentinelEcho(t *testing.T) {
	s, _ := newSession(t, func(string) (string, int) { return "the answer\n", 0 })
	defer s.Close()

	out, _, err := s.Run(context.Background(), "echo the answer", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "printf") || strings.Contains(out, s.sentinel) {
		t.Errorf("output leaked the protocol:\n%q", out)
	}
	if strings.Contains(out, "echo the answer") {
		t.Errorf("output contains the echoed command:\n%q", out)
	}
	if strings.TrimSpace(out) != "the answer" {
		t.Errorf("output = %q", out)
	}
}

// A command that never finishes must not block forever: Terminal has no read
// deadline, so without a background reader no timer could interrupt it.
//
// A timed-out session is also no longer at a known state — the command may
// still be running and its output would arrive in the middle of the next one —
// so it closes rather than silently interleaving two commands' output.
func TestShellSession_TimeoutClosesTheSession(t *testing.T) {
	// A terminal that never emits the sentinel: the command never finishes.
	term := newFakeTerminal(nil)
	s := newShellSession(term)
	defer s.Close()

	start := time.Now()
	_, code, err := s.Run(context.Background(), "sleep forever", 50*time.Millisecond)
	if err == nil {
		t.Fatal("the command did not time out")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the timeout took %v; the read was not interruptible", elapsed)
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1 for an unknown outcome", code)
	}
	if _, _, err := s.Run(context.Background(), "ls", time.Second); err == nil {
		t.Error("a timed-out session accepted another command")
	}
}

// floodBody is a command's output far past the retained cap: enough to force
// the middle to be dropped, with markers at both ends to prove the head and
// the tail survive.
func floodBody() string {
	return "first-line\n" + strings.Repeat("x", 3*sessionBufCap) + "\nlast-line\n"
}

// A command that floods the session must not buffer its whole output: the
// retained buffer is capped, the sentinel at the very end is still found, and
// the exit status still comes through.
func TestShellSession_FloodingCommandIsCappedAndCompletes(t *testing.T) {
	body := floodBody()
	s, _ := newSession(t, func(cmd string) (string, int) {
		if cmd == "flood" {
			return body, 7
		}
		return "", 0
	})
	defer s.Close()

	out, code, err := s.Run(context.Background(), "flood", 30*time.Second)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if len(out) > 2*sessionBufCap {
		t.Errorf("retained %d bytes of output, want at most %d", len(out), 2*sessionBufCap)
	}
	if !strings.Contains(out, "first-line") {
		t.Error("the output's head was dropped")
	}
	if !strings.Contains(out, "last-line") {
		t.Error("the output's tail was dropped")
	}
}

// The timeout path returns the partial output a flooding command produced,
// bounded by the same cap.
func TestShellSession_FloodTimeoutReturnsBoundedPartialOutput(t *testing.T) {
	body := floodBody()
	term := newFakeTerminal(func(string) (string, int) { return body, 0 })
	term.noSentinel = true // the command never finishes
	s := newShellSession(term)
	defer s.Close()

	out, code, err := s.Run(context.Background(), "flood", 200*time.Millisecond)
	if err == nil {
		t.Fatal("the command did not time out")
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1 for an unknown outcome", code)
	}
	if len(out) > 2*sessionBufCap {
		t.Errorf("retained %d bytes of partial output, want at most %d", len(out), 2*sessionBufCap)
	}
	if !strings.Contains(out, "first-line") {
		t.Error("the partial output lost its head")
	}
}

func TestShellSession_ClosedSessionRefuses(t *testing.T) {
	s, _ := newSession(t, func(string) (string, int) { return "", 0 })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Run(context.Background(), "ls", time.Second); err == nil {
		t.Error("a closed session accepted a command")
	}
	// Close is idempotent.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// fakeTerminalSandbox is a Sandbox that only knows how to open terminals — the
// one method the session pool uses. The first open can be parked on release, to
// hold one session's setup while another name is served.
type fakeTerminalSandbox struct {
	Sandbox // the pool never calls anything else

	started   chan struct{}
	opens     atomic.Int64
	firstOpen sync.Once
	release   chan struct{}

	mu    sync.Mutex
	terms []*fakeTerminal
}

func (f *fakeTerminalSandbox) OpenTerminal(ctx context.Context, _ TerminalOptions) (Terminal, error) {
	first := false
	f.firstOpen.Do(func() { first = true })
	f.opens.Add(1)
	f.started <- struct{}{}
	if first && f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	term := newFakeTerminal(func(string) (string, int) { return "", 0 })
	f.mu.Lock()
	f.terms = append(f.terms, term)
	f.mu.Unlock()
	return term, nil
}

// openTerminals reports how many of the terminals handed out are still open.
func (f *fakeTerminalSandbox) openTerminals() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, term := range f.terms {
		term.mu.Lock()
		if !term.closed {
			n++
		}
		term.mu.Unlock()
	}
	return n
}

// Opening a session is a network round-trip plus a PTY handshake on the ssh
// backend. It must not run under the pool lock: every OTHER named session's
// command would queue behind it.
func TestSessionPool_SlowOpenDoesNotBlockOtherNames(t *testing.T) {
	sb := &fakeTerminalSandbox{started: make(chan struct{}, 8), release: make(chan struct{})}
	p := newSessionPool()
	defer p.Close()
	// Released before p.Close runs (defers are LIFO): a held-open session would
	// otherwise leave the pool lock taken and turn a failure into a hang.
	release := sync.OnceFunc(func() { close(sb.release) })
	defer release()

	parked := make(chan error, 1)
	go func() {
		_, _, err := p.run(context.Background(), sb, "slow", "echo hi", 5*time.Second)
		parked <- err
	}()
	<-sb.started // the first open is in flight and held

	served := make(chan error, 1)
	go func() {
		_, _, err := p.run(context.Background(), sb, "other", "echo hi", 5*time.Second)
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("second session: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a command in a second session queued behind the first session's open")
	}

	release()
	if err := <-parked; err != nil {
		t.Fatalf("first session: %v", err)
	}
}

// Opening outside the lock means two callers can race the same new name. The
// pool must still end up with exactly one session under it.
func TestSessionPool_ConcurrentSameNameKeepsOneSession(t *testing.T) {
	sb := &fakeTerminalSandbox{started: make(chan struct{}, 64)}
	p := newSessionPool()
	defer p.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			if _, _, err := p.run(context.Background(), sb, "build", "echo hi", 5*time.Second); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("run: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) != 1 {
		t.Errorf("pool holds %d sessions for one name, want 1", len(p.sessions))
	}
}

// Close is a barrier: a session whose open was in flight must be closed too.
// Opening outside the pool lock is what makes this possible at all — the open
// lands in a pool that Close has already emptied, and the PTY (a remote ssh
// session on that backend) would be held for the life of the process, which is
// the leak CodeToolConfig.RegisterCloser exists to prevent.
func TestSessionPool_CloseClosesASessionOpenedConcurrently(t *testing.T) {
	sb := &fakeTerminalSandbox{started: make(chan struct{}, 8), release: make(chan struct{})}
	p := newSessionPool()

	ran := make(chan error, 1)
	go func() {
		_, _, err := p.run(context.Background(), sb, "build", "echo hi", 5*time.Second)
		ran <- err
	}()
	<-sb.started // the open is in flight, parked before it hands back a terminal

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(sb.release) // the parked open now lands in a closed pool

	if err := <-ran; err == nil {
		t.Error("a command ran in a session the pool opened after Close")
	}
	if n := sb.openTerminals(); n != 0 {
		t.Errorf("%d terminals still open after Close, want 0", n)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) != 0 {
		t.Errorf("pool holds %d sessions after Close, want 0", len(p.sessions))
	}
}

// The steady state: a named session is opened once and reused.
func TestSessionPool_ReusesTheNamedSession(t *testing.T) {
	sb := &fakeTerminalSandbox{started: make(chan struct{}, 8)}
	p := newSessionPool()
	defer p.Close()

	for range 3 {
		if _, _, err := p.run(context.Background(), sb, "build", "echo hi", 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if got := sb.opens.Load(); got != 1 {
		t.Errorf("opened %d terminals for one name, want 1", got)
	}
}
