package docker

import (
	"context"

	"github.com/zzir/agents-go/sandbox"
)

// Every file operation below dispatches the same way: a bind-mounted WorkDir
// runs on the host under an os.Root (files_host.go), Persistent goes through
// exec in the container (files_container.go), neither is sandbox.ErrNoWorkDir.

// ReadFile implements sandbox.Sandbox. Files larger than
// Options.MaxReadFileBytes (default sandbox.DefaultMaxReadFileBytes) fail
// with sandbox.ErrReadLimitExceeded instead of being loaded into host memory.
func (s *Sandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	switch {
	case s.opts.WorkDir != "":
		return s.readFileHost(p)
	case s.opts.Persistent:
		return s.readFileContainer(ctx, p)
	default:
		return nil, sandbox.ErrNoWorkDir
	}
}

// WriteFile implements sandbox.Sandbox.
func (s *Sandbox) WriteFile(ctx context.Context, p string, content []byte) error {
	switch {
	case s.opts.WorkDir != "":
		return s.writeFileHost(p, content)
	case s.opts.Persistent:
		return s.writeFileContainer(ctx, p, content)
	default:
		return sandbox.ErrNoWorkDir
	}
}

// CreateExclusive implements sandbox.Sandbox atomically: bind-mount mode uses
// O_EXCL under os.Root; persistent mode writes a temp file and publishes it with
// a hard link (ln fails with EEXIST if the target exists). The parent directory
// is created first so adding into a new directory works.
func (s *Sandbox) CreateExclusive(ctx context.Context, p string, content []byte) error {
	switch {
	case s.opts.WorkDir != "":
		return s.createExclusiveHost(p, content)
	case s.opts.Persistent:
		return s.createExclusiveContainer(ctx, p, content)
	default:
		return sandbox.ErrNoWorkDir
	}
}

// RemoveFile implements sandbox.Sandbox.
func (s *Sandbox) RemoveFile(ctx context.Context, p string) error {
	switch {
	case s.opts.WorkDir != "":
		return s.removeFileHost(p)
	case s.opts.Persistent:
		return s.removeFileContainer(ctx, p)
	default:
		return sandbox.ErrNoWorkDir
	}
}

// Rename implements sandbox.Sandbox.
func (s *Sandbox) Rename(ctx context.Context, oldPath, newPath string) error {
	switch {
	case s.opts.WorkDir != "":
		return s.renameHost(oldPath, newPath)
	case s.opts.Persistent:
		return s.renameContainer(ctx, oldPath, newPath)
	default:
		return sandbox.ErrNoWorkDir
	}
}

// ListDir implements sandbox.Sandbox.
func (s *Sandbox) ListDir(ctx context.Context, p string) ([]sandbox.DirEntry, error) {
	switch {
	case s.opts.WorkDir != "":
		return s.listDirHost(p)
	case s.opts.Persistent:
		return s.listDirContainer(ctx, p)
	default:
		return nil, sandbox.ErrNoWorkDir
	}
}
