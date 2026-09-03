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

// terminalOpTimeout bounds one daemon call by a Terminal method (and Close):
// the interface takes no context, so a dead daemon must not park a goroutine.
const terminalOpTimeout = 10 * time.Second

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
	// Tag the shell with a marker (as execPersistent does) so Close can kill the
	// whole process tree; dropping the attach alone leaves e.g. `top` alive.
	marker := newExecMarker()
	shell := opts.Shell
	if len(shell) == 0 {
		// Prefer bash when the image ships it, else POSIX sh; `command -v` works in
		// dash and busybox ash alike.
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

// terminal is an interactive TTY exec in the persistent container: with
// TTY=true the hijacked stream is raw, so Reader is merged output and Conn is stdin.
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
	ctx, cancel := context.WithTimeout(context.Background(), terminalOpTimeout)
	defer cancel()
	_, err := t.sb.cli.ExecResize(ctx, t.execID, client.ExecResizeOptions{
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
		ctx, cancel := context.WithTimeout(context.Background(), terminalOpTimeout)
		defer cancel()
		t.sb.killExec(ctx, t.containerID, t.marker)
		t.hijack.Close()
	})
	return nil
}

// Wait resolves the exit code after output EOF by polling ExecInspect briefly;
// -1 when the process is still reported running when the poll window closes.
// The window bounds the daemon calls as well as the sleeping, so a daemon that
// stops answering ends the wait unresolved instead of holding it forever.
func (t *terminal) Wait() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), terminalWaitPoll)
	defer cancel()
	for {
		inspect, err := t.sb.cli.ExecInspect(ctx, t.execID, client.ExecInspectOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return -1, nil // the window closed, same as still-running
			}
			return -1, fmt.Errorf("docker sandbox: exec inspect: %w", err)
		}
		if !inspect.Running {
			return inspect.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}
