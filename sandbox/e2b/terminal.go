package e2b

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

// OpenTerminal starts a shell on a PTY inside the sandbox. The stream that
// carries its output is envd's Start, opened with a pty size; input goes back
// as SendInput calls, and a resize as Update — the same three the JS and
// Python SDKs make.
func (s *Sandbox) OpenTerminal(ctx context.Context, opts sandbox.TerminalOptions) (sandbox.Terminal, error) {
	// The session is open-ended and there is no keepalive: the best available
	// is to open it on a freshly refreshed full lease.
	if _, err := s.ensureFor(ctx, time.Duration(s.timeout())*time.Second); err != nil {
		return nil, err
	}
	shell := opts.Shell
	if len(shell) == 0 {
		// Prefer bash when the image ships it (as the docker backend does): the
		// e2b image's /bin/sh is dash, which chokes on bash-style login profiles.
		shell = []string{"/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash -l || exec sh -l"}
	}
	envs := map[string]string{"TERM": opts.EffectiveTerm()}
	for k, v := range s.opts.Env {
		envs[k] = v
	}
	for k, v := range opts.Env {
		envs[k] = v
	}
	start := map[string]any{
		"process": map[string]any{
			"cmd":  shell[0],
			"args": shell[1:],
			"envs": envs,
			"cwd":  s.workDir(),
		},
		"pty": map[string]any{
			"size": map[string]any{"cols": opts.EffectiveCols(), "rows": opts.EffectiveRows()},
		},
	}

	// The session outlives this call, so it gets a context of its own: the
	// caller's bounds establishment only (the TerminalOpener contract).
	sessCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t := &terminal{
		sb:     s,
		cancel: cancel,
		out:    make(chan []byte, 16),
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
		exit:   -1,
	}
	go t.pump(sessCtx, start)

	// Wait for the shell to exist: a Terminal handed back before its pid is
	// known cannot take input, and the caller would write into a void.
	select {
	case <-t.ready:
		return t, nil
	case <-t.done:
		// The stream ended before a pid arrived — an error, or an immediate
		// exit. ready is closed ONLY on a real pid, so a done that beats it
		// means there is no live session to hand back; surface why instead of a
		// dead terminal whose failure hides in Wait().
		select {
		case <-t.ready:
			return t, nil // a pid did arrive, racing done — the session is valid
		default:
		}
		cancel()
		if t.err != nil {
			return nil, t.err
		}
		return nil, errors.New("e2b: the terminal stream ended before the shell started")
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

// terminal is one live PTY session.
type terminal struct {
	sb     *Sandbox
	cancel context.CancelFunc

	out   chan []byte
	ready chan struct{} // closed once pid is known
	done  chan struct{} // closed when the stream ended
	err   error

	mu        sync.Mutex
	pid       uint32
	buf       []byte
	exit      int
	closeOnce sync.Once
}

// pump runs the output stream for the session's lifetime.
func (t *terminal) pump(ctx context.Context, start map[string]any) {
	defer close(t.done)
	var readyOnce sync.Once
	err := t.sb.stream(ctx, procProcessStart, start, func(raw json.RawMessage) error {
		var ev processEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("e2b: decoding a terminal event: %w", err)
		}
		switch {
		case ev.Event.Start != nil:
			t.mu.Lock()
			t.pid = ev.Event.Start.PID
			t.mu.Unlock()
			readyOnce.Do(func() { close(t.ready) })
		case ev.Event.Data != nil && ev.Event.Data.PTY != "":
			// A chunk that will not decode is dropped, not fatal: losing a
			// few bytes of terminal output beats killing the session.
			chunk, derr := base64.StdEncoding.DecodeString(ev.Event.Data.PTY)
			if derr != nil {
				return nil //nolint:nilerr // see above
			}
			select {
			case t.out <- chunk:
			case <-ctx.Done():
				return ctx.Err()
			}
		case ev.Event.End != nil:
			t.mu.Lock()
			t.exit = ev.exitCode()
			t.mu.Unlock()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.err = err
	}
	// ready is deliberately NOT closed here: it closes only when a pid arrives,
	// so OpenTerminal can tell "shell started" from "stream ended first" (a
	// close here would hand back a terminal that is already dead).
	close(t.out)
}

// Read returns terminal output, and io.EOF once the shell has exited.
func (t *terminal) Read(p []byte) (int, error) {
	t.mu.Lock()
	if len(t.buf) > 0 {
		n := copy(p, t.buf)
		t.buf = t.buf[n:]
		t.mu.Unlock()
		return n, nil
	}
	t.mu.Unlock()
	chunk, ok := <-t.out
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, chunk)
	if n < len(chunk) {
		t.mu.Lock()
		t.buf = append(t.buf, chunk[n:]...)
		t.mu.Unlock()
	}
	return n, nil
}

// Write sends raw input to the PTY.
func (t *terminal) Write(p []byte) (int, error) {
	pid, ok := t.currentPID()
	if !ok {
		return 0, io.ErrClosedPipe
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancel()
	err := t.sb.unary(ctx, procSendInput, map[string]any{
		"process": map[string]any{"pid": pid},
		"input":   map[string]any{"pty": base64.StdEncoding.EncodeToString(p)},
	}, nil)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize changes the PTY size.
func (t *terminal) Resize(cols, rows int) error {
	pid, ok := t.currentPID()
	if !ok {
		return io.ErrClosedPipe
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancel()
	return t.sb.unary(ctx, procUpdate, map[string]any{
		"process": map[string]any{"pid": pid},
		"pty":     map[string]any{"size": map[string]any{"cols": cols, "rows": rows}},
	}, nil)
}

// Close ends the session: the shell is signalled and the stream torn down.
func (t *terminal) Close() error {
	t.closeOnce.Do(func() {
		if pid, ok := t.currentPID(); ok {
			t.sb.signal(context.Background(), pid, "SIGNAL_SIGKILL")
		}
		t.cancel()
	})
	return nil
}

// Wait blocks until the shell exited and returns its code, or -1 when the
// stream ended before an exit event.
func (t *terminal) Wait() (int, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, t.err
}

func (t *terminal) currentPID() (uint32, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pid, t.pid != 0
}
