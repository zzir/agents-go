package docker

import (
	"context"

	"github.com/zzir/agents-go/sandbox"
)

// ReadFile implements sandbox.Sandbox: a bind-mounted WorkDir reads on the
// host (files_host.go), Persistent through exec (files_container.go); a file
// over Options.MaxReadFileBytes fails with sandbox.ErrReadLimitExceeded.
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

// CreateExclusive implements sandbox.Sandbox atomically: O_EXCL under os.Root
// in bind-mount mode, a temp file published by hard link (EEXIST on an existing
// target) in persistent mode. Parent directories are created first.
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
