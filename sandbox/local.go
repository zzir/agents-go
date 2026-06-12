package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// localWaitDelay bounds how long Exec waits for the stdout/stderr pipes after
// the process exits or the timeout fires. Without it, a backgrounded
// grandchild that inherited the pipes would block Wait until it exits.
const localWaitDelay = 2 * time.Second

// LocalOptions configures LocalSandbox.
type LocalOptions struct {
	// InheritHostEnv passes the entire host environment to the command. By
	// default the command sees only PATH, HOME and TMPDIR from the host plus
	// ExecRequest.Env, so host secrets (API keys, tokens) cannot leak into
	// model-generated code.
	InheritHostEnv bool
}

// LocalSandbox runs commands directly on the host in a temporary working
// directory. It performs NO isolation beyond a minimal default environment
// (only PATH, HOME and TMPDIR from the host plus ExecRequest.Env, unless
// LocalOptions.InheritHostEnv is set) and must only be used for development
// and tests with trusted code — never for untrusted, agent-generated code in
// production. Use the docker or k8s backends for real isolation.
type LocalSandbox struct {
	opts LocalOptions
}

// NewLocal returns a LocalSandbox with default options.
func NewLocal() *LocalSandbox { return &LocalSandbox{} }

// NewLocalWithOptions returns a LocalSandbox with the given options.
func NewLocalWithOptions(opts LocalOptions) *LocalSandbox { return &LocalSandbox{opts: opts} }

// Exec implements Sandbox by running req.Cmd in a fresh temp directory.
//
// The command runs in its own process group (on Unix), and the whole group is
// killed when the timeout fires, so backgrounded grandchildren cannot outlive
// the deadline or hold the output pipes open indefinitely.
func (s *LocalSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("sandbox: ExecRequest.Cmd is empty")
	}
	dir, err := os.MkdirTemp("", "agents-sandbox-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	for name, content := range req.Files {
		full := filepath.Join(dir, filepath.Clean("/"+name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return nil, err
		}
	}

	cctx, cancel := context.WithTimeout(ctx, req.EffectiveTimeout())
	defer cancel()

	cmd := exec.CommandContext(cctx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = dir
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	cmd.Env = s.buildEnv(req.Env)
	maxOut := req.EffectiveMaxOutputBytes()
	stdout := &cappedBuffer{max: maxOut}
	stderr := &cappedBuffer{max: maxOut}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Lead a new process group and kill the whole group on cancellation.
	setProcessGroup(cmd)
	// Even when no grandchild is killed (or on non-Unix platforms), do not let
	// inherited pipes block Wait for longer than this after process exit.
	cmd.WaitDelay = localWaitDelay

	runErr := cmd.Run()
	if cmd.Process != nil {
		// Best-effort sweep of any processes left behind in the group.
		_ = killProcessGroup(cmd.Process.Pid)
	}
	res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The process itself exited successfully (a failure would surface as
		// *exec.ExitError) but something kept the output pipes open past
		// WaitDelay; treat it as a normal exit.
		runErr = nil
	}
	if runErr != nil && cctx.Err() == context.DeadlineExceeded {
		// Only a failed run that coincides with the deadline is a timeout: a
		// command that completed successfully is never reported as timed out.
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		res.ExitCode = exitErr.ExitCode()
	} else if runErr != nil {
		return nil, runErr
	}
	return res, nil
}

// buildEnv assembles the child environment: the full host environment when
// InheritHostEnv is set, otherwise a minimal set of host variables (PATH,
// HOME, TMPDIR), plus the request's Env in both cases.
func (s *LocalSandbox) buildEnv(reqEnv map[string]string) []string {
	env := make([]string, 0, 3+len(reqEnv))
	if s.opts.InheritHostEnv {
		env = append(env, os.Environ()...)
	} else {
		for _, k := range []string{"PATH", "HOME", "TMPDIR"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	for k, v := range reqEnv {
		env = append(env, k+"="+v)
	}
	return env
}

// cappedBuffer is an io.Writer that keeps at most max bytes and silently
// discards the rest, so a runaway process cannot exhaust memory and never
// sees a write error.
type cappedBuffer struct {
	buf bytes.Buffer
	max int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remain := b.max - int64(b.buf.Len()); remain > 0 {
		if int64(len(p)) > remain {
			b.buf.Write(p[:remain])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// Close implements Sandbox.
func (s *LocalSandbox) Close() error { return nil }

var _ Sandbox = (*LocalSandbox)(nil)
