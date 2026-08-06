package docker

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzir/agents-go/sandbox"
)

// The host side of the file operations, used when Options.WorkDir bind-mounts a
// host directory into the container: these run on the HOST filesystem, outside
// the container's isolation, so every path is resolved through an os.Root
// opened on WorkDir (see rootRel).

// hostRoot opens the os.Root guarding the bind-mounted working directory and
// resolves p to a name relative to it. The caller closes the root.
func (s *Sandbox) hostRoot(p string) (*os.Root, string, error) {
	rel, err := rootRel(p)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(s.opts.WorkDir)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

// mkdirParents creates rel's parent directories inside root, if any.
func mkdirParents(root *os.Root, rel string) error {
	dir := filepath.Dir(rel)
	if dir == "." {
		return nil
	}
	return root.MkdirAll(dir, 0o755)
}

func (s *Sandbox) readFileHost(p string) ([]byte, error) {
	root, rel, err := s.hostRoot(p)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return sandbox.ReadAllLimited(f, s.opts.MaxReadFileBytes)
}

func (s *Sandbox) writeFileHost(p string, content []byte) error {
	root, rel, err := s.hostRoot(p)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := mkdirParents(root, rel); err != nil {
		return err
	}
	return root.WriteFile(rel, content, 0o644)
}

func (s *Sandbox) createExclusiveHost(p string, content []byte) error {
	root, rel, err := s.hostRoot(p)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := mkdirParents(root, rel); err != nil {
		return err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if werr := writeAndClose(f, content); werr != nil {
		// Don't leave a partial file the caller believes was rolled back.
		_ = root.Remove(rel)
		return werr
	}
	return nil
}

// writeAndClose writes content to f and closes it, reporting the first failure.
func writeAndClose(f *os.File, content []byte) error {
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *Sandbox) removeFileHost(p string) error {
	root, rel, err := s.hostRoot(p)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(rel)
}

func (s *Sandbox) renameHost(oldPath, newPath string) error {
	// Both names are validated before the root is opened, so a bad destination
	// is rejected the same way whether or not WorkDir can be opened.
	from, err := rootRel(oldPath)
	if err != nil {
		return err
	}
	to, err := rootRel(newPath)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(s.opts.WorkDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := mkdirParents(root, to); err != nil {
		return err
	}
	return root.Rename(from, to)
}

func (s *Sandbox) listDirHost(p string) ([]sandbox.DirEntry, error) {
	root, rel, err := s.hostRoot(p)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	out := make([]sandbox.DirEntry, 0, len(entries))
	for _, e := range entries {
		info, ierr := e.Info()
		var size int64
		if ierr == nil {
			size = info.Size()
		}
		out = append(out, sandbox.DirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  size,
		})
	}
	return out, nil
}

// rootRel maps a model-supplied path into a name relative to the os.Root
// opened on the bind-mounted working directory. Bind-mount mode is the one
// place the file tools do NOT share exec's view: they run on the HOST side of
// the mount, where the container's isolation cannot cover them, so everything
// must stay inside WorkDir. A relative path passes through — os.Root itself
// rejects any ".." or symlink escape — and an absolute path must lie under
// the in-container mount point (/workspace, the only view the model ever
// sees) and is translated to its host-side name. Anything else is refused
// with sandbox.ErrOutsideWorkDir rather than silently re-rooted.
func rootRel(p string) (string, error) {
	if !path.IsAbs(p) {
		if p == "" {
			return ".", nil
		}
		return filepath.FromSlash(p), nil
	}
	clean := path.Clean(p)
	if clean == workDir {
		return ".", nil
	}
	if rest, ok := strings.CutPrefix(clean, workDir+"/"); ok {
		return filepath.FromSlash(rest), nil
	}
	return "", fmt.Errorf("docker sandbox: %q is outside the working directory %s: %w", p, workDir, sandbox.ErrOutsideWorkDir)
}
