package docker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/zzir/agents-go/sandbox"
)

var _ sandbox.TerminalOpener = (*Sandbox)(nil)

// terminalWaitPoll bounds how long Wait polls ExecInspect for the exit code
// after output EOF; Docker has no exec-wait API.
const terminalWaitPoll = 2 * time.Second

// OpenTerminal implements sandbox.TerminalOpener. Persistent mode only: an
// interactive shell needs a long-lived container to attach to; in ephemeral
// mode there is no container between Exec calls.
func (s *Sandbox) OpenTerminal(ctx context.Context, opts sandbox.TerminalOptions) (sandbox.Terminal, error) {
	if !s.opts.Persistent {
		return nil, fmt.Errorf("docker sandbox: %w: interactive terminals require Persistent mode", sandbox.ErrTerminalUnsupported)
	}
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	id, err := s.ensureContainer(ctx)
	if err != nil {
		return nil, err
	}
	// Tag the shell with a marker (as execPersistent does) so Close can kill
	// the whole process tree inside the container: dropping the attach
	// connection alone leaves e.g. a running `top` alive.
	marker := newExecMarker()
	shell := opts.Shell
	if len(shell) == 0 {
		// Prefer bash when the image ships it — tab completion, history and
		// line editing out of the box — and fall back to POSIX sh. `command -v`
		// works in dash and busybox ash alike.
		shell = []string{"sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash -l || exec sh -l"}
	}
	size := client.ConsoleSize{Height: uint(opts.EffectiveRows()), Width: uint(opts.EffectiveCols())}
	created, err := s.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd:          shell,
		WorkingDir:   workDir,
		Env:          append(envSlice(opts.Env), "TERM="+opts.EffectiveTerm(), execMarkerEnv+"="+marker),
		TTY:          true,
		ConsoleSize:  size,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec create: %w", err)
	}
	// ctx bounds the attach handshake only; the hijacked connection lives
	// until Close, matching the TerminalOpener contract.
	attached, err := s.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: size})
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: exec attach: %w", err)
	}
	return &terminal{
		sb:          s,
		containerID: id,
		execID:      created.ID,
		marker:      marker,
		hijack:      attached.HijackedResponse,
	}, nil
}

// terminal is an interactive TTY exec in the persistent container. With
// TTY=true the hijacked stream is raw — no stdcopy multiplexing — so Reader
// carries the merged output and Conn accepts stdin bytes directly.
type terminal struct {
	sb          *Sandbox
	containerID string
	execID      string
	marker      string
	hijack      client.HijackedResponse

	closeOnce sync.Once
}

func (t *terminal) Read(p []byte) (int, error)  { return t.hijack.Reader.Read(p) }
func (t *terminal) Write(p []byte) (int, error) { return t.hijack.Conn.Write(p) }

func (t *terminal) Resize(cols, rows int) error {
	_, err := t.sb.cli.ExecResize(context.Background(), t.execID, client.ExecResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
	return err
}

// Close kills the shell's process tree via the exec marker (a no-op when the
// shell already exited) and drops the hijacked connection, unblocking any
// concurrent Read.
func (t *terminal) Close() error {
	t.closeOnce.Do(func() {
		t.sb.killExec(context.Background(), t.containerID, t.marker)
		t.hijack.Close()
	})
	return nil
}

// Wait resolves the exit code after output EOF by polling ExecInspect briefly;
// -1 when the process is still reported running when the poll window closes.
func (t *terminal) Wait() (int, error) {
	deadline := time.Now().Add(terminalWaitPoll)
	for {
		inspect, err := t.sb.cli.ExecInspect(context.Background(), t.execID, client.ExecInspectOptions{})
		if err != nil {
			return -1, fmt.Errorf("docker sandbox: exec inspect: %w", err)
		}
		if !inspect.Running {
			return inspect.ExitCode, nil
		}
		if time.Now().After(deadline) {
			return -1, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}
