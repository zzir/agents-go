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

// The host side of the file operations (Options.WorkDir bind-mounted): these
// run on the HOST, so every path goes through an os.Root on WorkDir (decisions §5.14).

// hostRoot opens the os.Root guarding the bind-mounted working directory and
// resolves p to a name relative to it. The caller closes the root.
func (s *Sandbox) hostRoot(p string) (*os.Root, string, error) {
	rel, err := s.rootRel(p)
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
	if werr := sandbox.WriteAndClose(f, content); werr != nil {
		_ = root.Remove(rel) // no partial file the caller believes was rolled back
		return werr
	}
	return nil
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
	from, err := s.rootRel(oldPath)
	if err != nil {
		return err
	}
	to, err := s.rootRel(newPath)
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

// rootRel maps a model-supplied path to a name under the os.Root: relative
// passes through, absolute must lie under /workspace, else ErrOutsideWorkDir.
func (s *Sandbox) rootRel(p string) (string, error) {
	if !path.IsAbs(p) {
		return filepath.FromSlash(path.Clean(p)), nil
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
