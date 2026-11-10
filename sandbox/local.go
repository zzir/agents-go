package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// localWaitDelay bounds how long ExecStream waits for the output pipes after
// exit; a backgrounded grandchild holding them would otherwise block Wait.
const localWaitDelay = 2 * time.Second

// LocalSandbox runs commands directly on the host with NO isolation beyond a
// minimal environment: PATH, HOME and TMPDIR plus the request's. Development
// and trusted code only; docker or e2b isolate for real.
type LocalSandbox struct {
	// WorkDir is the working directory of every execution and file operation.
	// Empty means one temp directory, made on first use and removed by Close
	// (decisions §5.113).
	WorkDir string
	// MaxReadFileBytes caps ReadFile; a larger file fails with
	// ErrReadLimitExceeded. Zero (or negative) means DefaultMaxReadFileBytes.
	MaxReadFileBytes int64

	mu  sync.Mutex
	tmp string // the temp working directory, once made
}

// dir returns the working directory, making the temp one on first use.
func (s *LocalSandbox) dir() (string, error) {
	if s.WorkDir != "" {
		return s.WorkDir, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tmp == "" {
		tmp, err := os.MkdirTemp("", "agents-sandbox-")
		if err != nil {
			return "", err
		}
		s.tmp = tmp
	}
	return s.tmp, nil
}

// ExecStream implements Sandbox. The command leads its own process group
// (Unix), killed whole when the timeout fires.
func (s *LocalSandbox) ExecStream(ctx context.Context, req ExecRequest, stdout, stderr io.Writer) (*ExecResult, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("sandbox: ExecRequest.Cmd is empty")
	}
	dir, err := s.dir()
	if err != nil {
		return nil, err
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
	cmd.Env = buildEnv(req.Env)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Lead a new process group and kill the whole group on cancellation.
	setProcessGroup(cmd)
	// Even when no grandchild is killed (or on non-Unix platforms), do not let
	// inherited pipes block Wait for longer than this after process exit.
	cmd.WaitDelay = localWaitDelay

	runErr := cmd.Run()
	if cctx.Err() != nil && cmd.Process != nil {
		// Sweep the group ONLY after the context fired: after a normal exit the
		// leader's PID (and so the group id) may already belong to someone else.
		_ = killProcessGroup(cmd.Process.Pid)
	}
	res := &ExecResult{}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The process exited fine; something kept the pipes open past WaitDelay.
		runErr = nil
	}
	// The CALLER's ending is checked first, else its deadline would read as this
	// command's timeout (spec §2.7m).
	if runErr != nil && ctx.Err() != nil {
		return nil, ctx.Err()
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

// resolve maps a model-supplied path with shell semantics, mirroring exec: an
// absolute path as-is, a relative one under the working directory (spec §2.7t).
func (s *LocalSandbox) resolve(p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	dir, err := s.dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, p), nil
}

// ReadFile implements Sandbox; a file over MaxReadFileBytes fails with
// ErrReadLimitExceeded.
func (s *LocalSandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	full, err := s.resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadAllLimited(f, s.MaxReadFileBytes)
}

// WriteFile implements Sandbox.
func (s *LocalSandbox) WriteFile(_ context.Context, p string, content []byte) error {
	full, err := s.resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, content, 0o644)
}

// CreateExclusive implements Sandbox with O_EXCL, so two creates of one path
// cannot both succeed.
func (s *LocalSandbox) CreateExclusive(_ context.Context, p string, content []byte) error {
	full, err := s.resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err // fs.ErrExist when it already exists
	}
	if werr := WriteAndClose(f, content); werr != nil {
		_ = os.Remove(full) // no partial file the caller believes was rolled back
		return werr
	}
	return nil
}

// RemoveFile implements Sandbox.
func (s *LocalSandbox) RemoveFile(_ context.Context, p string) error {
	full, err := s.resolve(p)
	if err != nil {
		return err
	}
	return os.Remove(full)
}

// Rename implements Sandbox.
func (s *LocalSandbox) Rename(_ context.Context, oldPath, newPath string) error {
	from, err := s.resolve(oldPath)
	if err != nil {
		return err
	}
	to, err := s.resolve(newPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// ListDir implements Sandbox.
func (s *LocalSandbox) ListDir(_ context.Context, p string) ([]DirEntry, error) {
	full, err := s.resolve(p)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
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

// buildEnv is the child environment: the host's PATH, HOME and TMPDIR, plus
// the request's Env.
func buildEnv(reqEnv map[string]string) []string {
	env := make([]string, 0, 3+len(reqEnv))
	for _, k := range []string{"PATH", "HOME", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range reqEnv {
		env = append(env, k+"="+v)
	}
	return env
}

// Close implements Sandbox, removing the temp working directory if one was made.
func (s *LocalSandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tmp == "" {
		return nil
	}
	tmp := s.tmp
	s.tmp = ""
	return os.RemoveAll(tmp)
}

var _ Sandbox = (*LocalSandbox)(nil)
