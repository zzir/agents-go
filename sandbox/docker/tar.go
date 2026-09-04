package docker

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"time"
)

// buildTar packs files into a tar stream extracted at workDir, staging
// ExecRequest.Files and WriteFile's payload; nested parents are world-writable.
func buildTar(files map[string]string) (io.Reader, error) {
	names := make([]string, 0, len(files))
	clean := make(map[string]string, len(files))
	dirSet := map[string]bool{}
	for name, content := range files {
		cn := path.Clean("/" + name)[1:] // strip leading slash, prevent traversal
		if cn == "" {
			return nil, fmt.Errorf("docker sandbox: invalid file path %q", name)
		}
		if _, dup := clean[cn]; dup {
			return nil, fmt.Errorf("docker sandbox: duplicate file path %q", cn)
		}
		clean[cn] = content
		names = append(names, cn)
		for d := path.Dir(cn); d != "."; d = path.Dir(d) {
			dirSet[d] = true
		}
	}
	slices.Sort(names)
	dirs := slices.Sorted(maps.Keys(dirSet)) // parents sort before children

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeDir := func(name string, mode int64) error {
		return tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeDir,
			Mode:     mode,
			ModTime:  time.Unix(0, 0),
		})
	}
	for _, d := range dirs {
		if err := writeDir(d, 0o777); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		content := clean[name]
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(content)),
			ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}
