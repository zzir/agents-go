package docker

import (
	"context"

	"github.com/zzir/agents-go/sandbox"
)

// The file operations have two backends, and every public method below is the
// same three-way dispatch:
//
//   - Options.WorkDir set — the working directory is a bind-mounted HOST
//     directory, so the operation runs on the host under an os.Root guard
//     (files_host.go).
//   - Options.Persistent — the working directory lives inside the long-lived
//     container, reached with exec and tar (files_container.go).
//   - neither — there is no directory that outlives a call: sandbox.ErrNoWorkDir.
//
// The dispatch stays written out in each method on purpose: an interface over
// two backends that are never chosen dynamically would hide which one runs
// without removing a single branch.

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
