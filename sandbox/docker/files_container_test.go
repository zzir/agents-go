package docker

import (
	"archive/tar"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zzir/agents-go/sandbox"
)

// archiveEntry is what fakeArchiveDaemon serves for one in-container path.
type archiveEntry struct {
	typeflag byte
	linkname string
	content  string
}

// fakeArchiveDaemon serves the API slice readFileContainer touches, answering
// GET archive requests from entries and recording every requested path.
func fakeArchiveDaemon(t *testing.T, entries map[string]archiveEntry) (host string, got *[]string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	statHdr := base64.StdEncoding.EncodeToString([]byte("{}"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(p, "/_ping"):
			w.Header().Set("API-Version", "1.44")
		case strings.HasSuffix(p, "/images/img/json"):
			_, _ = w.Write([]byte("{}"))
		case strings.HasSuffix(p, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"cid"}`))
		case strings.HasSuffix(p, "/containers/cid/json"):
			_, _ = w.Write([]byte(`{"Id":"cid","State":{"Running":true}}`))
		case strings.HasSuffix(p, "/containers/cid/archive") && r.Method == http.MethodGet:
			src := r.URL.Query().Get("path")
			mu.Lock()
			paths = append(paths, src)
			mu.Unlock()
			e, ok := entries[src]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Could not find the file"}`))
				return
			}
			w.Header().Set("X-Docker-Container-Path-Stat", statHdr)
			tw := tar.NewWriter(w)
			_ = tw.WriteHeader(&tar.Header{
				Name:     path.Base(src),
				Typeflag: e.typeflag,
				Linkname: e.linkname,
				Mode:     0o644,
				Size:     int64(len(e.content)),
			})
			_, _ = tw.Write([]byte(e.content))
			_ = tw.Close()
		case strings.HasSuffix(p, "/containers/cid/archive"),
			strings.HasSuffix(p, "/containers/cid/start"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String(), &paths
}

// The daemon's CopyFromContainer neither errors on a directory nor follows a
// symlink at the source path — it just tars the header. ReadFile must match the
// local/ssh backends: a directory is an error, a symlink is followed (bounded),
// and the read limit applies to the file finally reached.
func TestReadFileContainer_DirAndSymlink(t *testing.T) {
	host, paths := fakeArchiveDaemon(t, map[string]archiveEntry{
		"/workspace/somedir":  {typeflag: tar.TypeDir},
		"/workspace/abs":      {typeflag: tar.TypeSymlink, linkname: "/etc/target"},
		"/etc/target":         {typeflag: tar.TypeReg, content: "resolved-abs"},
		"/workspace/sub/rel":  {typeflag: tar.TypeSymlink, linkname: "../real.txt"},
		"/workspace/real.txt": {typeflag: tar.TypeReg, content: "resolved-rel"},
		"/workspace/loop-a":   {typeflag: tar.TypeSymlink, linkname: "loop-b"},
		"/workspace/loop-b":   {typeflag: tar.TypeSymlink, linkname: "loop-a"},
		"/workspace/lim":      {typeflag: tar.TypeSymlink, linkname: "big"},
		"/workspace/big":      {typeflag: tar.TypeReg, content: strings.Repeat("x", 64)},
	})
	sb, err := New(Options{Host: host, Image: "img", Persistent: true, MaxReadFileBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	ctx := t.Context()

	t.Run("directory", func(t *testing.T) {
		_, err := sb.ReadFile(ctx, "somedir")
		if err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Errorf("ReadFile(somedir) err = %v, want an is-a-directory error", err)
		}
	})
	t.Run("absolute symlink", func(t *testing.T) {
		data, err := sb.ReadFile(ctx, "abs")
		if err != nil || string(data) != "resolved-abs" {
			t.Errorf("ReadFile(abs) = %q, %v; want the target's content", data, err)
		}
		if !slices.Contains(*paths, "/etc/target") {
			t.Errorf("requested paths %v, want the absolute target fetched", *paths)
		}
	})
	t.Run("relative symlink", func(t *testing.T) {
		data, err := sb.ReadFile(ctx, "sub/rel")
		if err != nil || string(data) != "resolved-rel" {
			t.Errorf("ReadFile(sub/rel) = %q, %v; want the target resolved against the link's dir", data, err)
		}
	})
	t.Run("symlink loop", func(t *testing.T) {
		_, err := sb.ReadFile(ctx, "loop-a")
		if err == nil || !strings.Contains(err.Error(), "too many levels of symbolic links") {
			t.Errorf("ReadFile(loop-a) err = %v, want a symlink-loop error", err)
		}
	})
	t.Run("read limit through symlink", func(t *testing.T) {
		if _, err := sb.ReadFile(ctx, "lim"); !errors.Is(err, sandbox.ErrReadLimitExceeded) {
			t.Errorf("ReadFile(lim) err = %v, want sandbox.ErrReadLimitExceeded", err)
		}
	})
}
