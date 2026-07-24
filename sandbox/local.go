package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

	// WorkDir, when non-empty, is used as the working directory for every
	// execution instead of a fresh temp directory. Request files are still
	// written into it, but the directory is NOT removed afterwards. This
	// allows the sandbox to operate on an existing project tree.
	WorkDir string

	// MaxReadFileBytes caps how many bytes ReadFile returns; larger files fail
	// with ErrReadLimitExceeded instead of being loaded into memory. Zero (or
	// negative) means DefaultMaxReadFileBytes.
	MaxReadFileBytes int64
}

// LocalSandbox runs commands directly on the host in a temporary working
// directory. It performs NO isolation beyond a minimal default environment
// (only PATH, HOME and TMPDIR from the host plus ExecRequest.Env, unless
// LocalOptions.InheritHostEnv is set) and must only be used for development
// and tests with trusted code — never for untrusted, agent-generated code in
// production. Use the docker backend for real isolation.
type LocalSandbox struct {
	opts LocalOptions
}

// NewLocal returns a LocalSandbox with default options.
func NewLocal() *LocalSandbox { return &LocalSandbox{} }

// NewLocalWithOptions returns a LocalSandbox with the given options.
func NewLocalWithOptions(opts LocalOptions) *LocalSandbox { return &LocalSandbox{opts: opts} }

// exec runs req.Cmd in a working directory, wiring stdout/stderr to the
// provided writers.
//
// The command runs in its own process group (on Unix), and the whole group is
// killed when the timeout fires, so backgrounded grandchildren cannot outlive
// the deadline or hold the output pipes open indefinitely.
func (s *LocalSandbox) exec(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (*ExecResult, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("sandbox: ExecRequest.Cmd is empty")
	}

	var dir string
	if s.opts.WorkDir != "" {
		dir = s.opts.WorkDir
	} else {
		tmp, err := os.MkdirTemp("", "agents-sandbox-")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}

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
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Lead a new process group and kill the whole group on cancellation.
	setProcessGroup(cmd)
	// Even when no grandchild is killed (or on non-Unix platforms), do not let
	// inherited pipes block Wait for longer than this after process exit.
	cmd.WaitDelay = localWaitDelay

	runErr := cmd.Run()
	if cctx.Err() != nil && cmd.Process != nil {
		// Best-effort sweep of any processes left behind in the group, but ONLY
		// after the context fired (timeout or cancellation), i.e. when cmd.Cancel
		// already SIGKILLed the group and stragglers are plausible. After a
		// normal exit the group leader has been reaped, so its PID — and thus the
		// process-group ID — may already have been reused; an unconditional
		// sweep here could SIGKILL an unrelated process group.
		_ = killProcessGroup(cmd.Process.Pid)
	}
	res := &ExecResult{}
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

// Exec runs a command and returns its buffered output.
func (s *LocalSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	maxOut := req.EffectiveMaxOutputBytes()
	stdoutBuf := &CappedBuffer{Max: maxOut}
	stderrBuf := &CappedBuffer{Max: maxOut}
	res, err := s.exec(ctx, req, stdoutBuf, stderrBuf)
	if err != nil {
		return nil, err
	}
	res.Stdout = stdoutBuf.String()
	res.Stderr = stderrBuf.String()
	return res, nil
}

// ExecStream implements ExecStreamer by streaming output to the provided writers.
func (s *LocalSandbox) ExecStream(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (*ExecResult, error) {
	return s.exec(ctx, req, stdout, stderr)
}

// ReadFile reads a file relative to the sandbox working directory. Files
// larger than LocalOptions.MaxReadFileBytes (default DefaultMaxReadFileBytes)
// fail with ErrReadLimitExceeded. Access is confined to WorkDir via os.Root, so
// a symlink under WorkDir cannot be followed to read a file outside it.
func (s *LocalSandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	if s.opts.WorkDir == "" {
		return nil, ErrNoWorkDir
	}
	root, err := os.OpenRoot(s.opts.WorkDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rootRel(p))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadAllLimited(f, s.opts.MaxReadFileBytes)
}

// WriteFile writes a file relative to the sandbox working directory, creating
// parent directories. Access is confined to WorkDir via os.Root, so a symlink
// under WorkDir cannot be followed to write outside it.
func (s *LocalSandbox) WriteFile(_ context.Context, p string, content []byte) error {
	if s.opts.WorkDir == "" {
		return ErrNoWorkDir
	}
	root, err := os.OpenRoot(s.opts.WorkDir)
	if err != nil {
		return err
	}
	defer root.Close()
	rel := rootRel(p)
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return root.WriteFile(rel, content, 0o644)
}

// RemoveFile removes a file relative to the sandbox working directory. Access
// is confined to WorkDir via os.Root; removing a symlink deletes the link, not
// its (possibly external) target.
func (s *LocalSandbox) RemoveFile(_ context.Context, p string) error {
	if s.opts.WorkDir == "" {
		return ErrNoWorkDir
	}
	root, err := os.OpenRoot(s.opts.WorkDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(rootRel(p))
}

// Rename moves a file within the sandbox working directory, creating the
// destination's parent directories. Access is confined to WorkDir via os.Root,
// so neither endpoint can escape it through a symlink.
func (s *LocalSandbox) Rename(_ context.Context, oldPath, newPath string) error {
	if s.opts.WorkDir == "" {
		return ErrNoWorkDir
	}
	root, err := os.OpenRoot(s.opts.WorkDir)
	if err != nil {
		return err
	}
	defer root.Close()
	to := rootRel(newPath)
	if dir := filepath.Dir(to); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return root.Rename(rootRel(oldPath), to)
}

// ListDir lists entries in a directory relative to the sandbox working
// directory. Access is confined to WorkDir via os.Root, so a symlinked
// subdirectory cannot be followed out of it. Entries are sorted by name.
func (s *LocalSandbox) ListDir(_ context.Context, p string) ([]DirEntry, error) {
	if s.opts.WorkDir == "" {
		return nil, ErrNoWorkDir
	}
	root, err := os.OpenRoot(s.opts.WorkDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rootRel(p))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		result = append(result, DirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	return result, nil
}

// rootRel converts a sandbox-relative path — which may carry a leading slash or
// ".." components — into a clean path relative to an os.Root. Cleaning as if
// rooted at "/" neutralizes any ".." that would escape, and the leading
// separator is then stripped because os.Root rejects rooted names. An empty or
// root-only path becomes ".", i.e. the working directory itself.
func rootRel(p string) string {
	rel := strings.TrimPrefix(filepath.Clean("/"+p), string(filepath.Separator))
	if rel == "" {
		return "."
	}
	return rel
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

// Close implements Sandbox.
func (s *LocalSandbox) Close() error { return nil }

var _ Sandbox = (*LocalSandbox)(nil)
var _ ExecStreamer = (*LocalSandbox)(nil)
