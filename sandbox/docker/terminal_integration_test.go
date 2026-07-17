//go:build docker_integration

package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

// termOutput drains a Terminal in a background goroutine so tests can wait
// for expected output (or EOF) with a deadline instead of blocking on Read.
type termOutput struct {
	t   *testing.T
	ch  chan termChunk
	buf strings.Builder
}

type termChunk struct {
	data []byte
	err  error
}

func newTermOutput(t *testing.T, term sandbox.Terminal) *termOutput {
	t.Helper()
	o := &termOutput{t: t, ch: make(chan termChunk, 64)}
	go func() {
		for {
			buf := make([]byte, 4096)
			n, err := term.Read(buf)
			o.ch <- termChunk{data: buf[:n], err: err}
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *termOutput) waitFor(want string, timeout time.Duration) {
	o.t.Helper()
	deadline := time.After(timeout)
	for {
		if strings.Contains(o.buf.String(), want) {
			return
		}
		select {
		case c := <-o.ch:
			o.buf.Write(c.data)
			if c.err != nil && !strings.Contains(o.buf.String(), want) {
				o.t.Fatalf("terminal closed before %q appeared (err: %v); output:\n%s", want, c.err, o.buf.String())
			}
		case <-deadline:
			o.t.Fatalf("timeout waiting for %q; output:\n%s", want, o.buf.String())
		}
	}
}

func (o *termOutput) waitEOF(timeout time.Duration) {
	o.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case c := <-o.ch:
			o.buf.Write(c.data)
			if c.err != nil {
				return
			}
		case <-deadline:
			o.t.Fatalf("timeout waiting for EOF; output:\n%s", o.buf.String())
		}
	}
}

func TestDockerSandbox_Terminal(t *testing.T) {
	sb, err := New(Options{Image: "python:3.12-slim", Persistent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	term, err := sb.OpenTerminal(context.Background(), sandbox.TerminalOptions{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	out := newTermOutput(t, term)

	// $((6*7)) keeps the expected string out of the PTY echo of the command
	// line itself, so a match proves the shell actually ran it.
	if _, err := term.Write([]byte("echo terminal-$((6*7))\n")); err != nil {
		t.Fatal(err)
	}
	out.waitFor("terminal-42", 15*time.Second)

	if err := term.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	out.waitFor("40 120", 10*time.Second)

	if _, err := term.Write([]byte("exit 5\n")); err != nil {
		t.Fatal(err)
	}
	out.waitEOF(10 * time.Second)
	code, err := term.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
}

// The default shell detection must pick bash when the image ships it
// (python:3.12-slim does) — that is what unlocks tab completion and history.
func TestDockerSandbox_TerminalPrefersBash(t *testing.T) {
	sb, err := New(Options{Image: "python:3.12-slim", Persistent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	term, err := sb.OpenTerminal(context.Background(), sandbox.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	out := newTermOutput(t, term)

	if _, err := term.Write([]byte(`[ -n "$BASH_VERSION" ] && echo shell-is-bash` + "\n")); err != nil {
		t.Fatal(err)
	}
	out.waitFor("shell-is-bash", 15*time.Second)
}

// TestDockerSandbox_TerminalCloseKillsShell verifies Close reaps the shell's
// process tree inside the container (via the exec marker), not just the
// attach connection.
func TestDockerSandbox_TerminalCloseKillsShell(t *testing.T) {
	sb, err := New(Options{Image: "python:3.12-slim", Persistent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()

	term, err := sb.OpenTerminal(context.Background(), sandbox.TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := newTermOutput(t, term)
	if _, err := term.Write([]byte("echo ready-$((1+1))\n")); err != nil {
		t.Fatal(err)
	}
	out.waitFor("ready-2", 15*time.Second)

	if err := term.Close(); err != nil {
		t.Fatal(err)
	}

	// The shell must be gone shortly after Close; poll via a fresh Exec.
	// Count shell processes (sh or bash — the default detection prefers bash)
	// straight from /proc; the slim image has no ps(1).
	countShells := `n=0; for d in /proc/[0-9]*; do c=$(cat "$d/comm" 2>/dev/null); if [ "$c" = "sh" ] || [ "$c" = "bash" ]; then n=$((n+1)); fi; done; echo "$n"`
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
			Cmd: []string{"sh", "-c", countShells},
		})
		if err != nil {
			t.Fatal(err)
		}
		// One shell is this probe's own sh; more means the terminal survived.
		if strings.TrimSpace(res.Stdout) == "1" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal shell still running after Close: %q", res.Stdout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
