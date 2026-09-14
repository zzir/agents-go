package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
)

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("inflating body: %v", err)
	}
	return string(raw)
}

// API responses from gzipMinLength up go out gzip-compressed to a client that
// accepts it; below it, on the replay stream and outside the API, they go out
// as they are — the floor would hold the stream's events back until it ended.
func TestAPIResponsesAreGzippedFromTheFloor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), staticAuth("tok"), nil)
	big := strings.Repeat("x", 4<<10)
	s.Engine.GET(APIPrefix+"/big", func(c *gin.Context) { c.String(http.StatusOK, big) })
	s.Engine.GET(APIPrefix+"/small", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	s.Engine.POST(APIPrefix+"/playground/generate", func(c *gin.Context) { c.String(http.StatusOK, big) })
	s.Engine.GET("/big", func(c *gin.Context) { c.String(http.StatusOK, big) })

	do := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		s.Engine.ServeHTTP(w, r)
		return w
	}
	w := do(http.MethodGet, APIPrefix+"/big")
	if w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("big response headers = %v, want gzip with Vary", w.Header())
	}
	if got := gunzip(t, w.Body.Bytes()); got != big {
		t.Fatalf("inflated body = %d bytes, want the %d written", len(got), len(big))
	}
	if w.Body.Len() >= len(big) {
		t.Fatalf("gzipped body is %d bytes, not smaller than %d", w.Body.Len(), len(big))
	}
	if w := do(http.MethodGet, APIPrefix+"/small"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != "ok" {
		t.Fatalf("small response = %q %v, want it as written", w.Body.String(), w.Header())
	}
	if w := do(http.MethodGet, "/big"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != big {
		t.Fatalf("non-API response = %d bytes %v, want it uncompressed", w.Body.Len(), w.Header())
	}
	if w := do(http.MethodPost, APIPrefix+"/playground/generate"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != big {
		t.Fatalf("replay stream = %d bytes %v, want it uncompressed", w.Body.Len(), w.Header())
	}
}

// A pre-compressed asset goes out as the bytes it is — labeled gzip once,
// never compressed twice — and inflated for a client that accepts none.
func TestPrecompressedAssetsAreServedOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), staticAuth("tok"), nil)
	html := "<!doctype html>" + strings.Repeat("<p>x</p>", 1<<10)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(html))
	_ = zw.Close()
	s.ServeStatic(fstest.MapFS{"index.html.gz": &fstest.MapFile{Data: buf.Bytes()}})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	s.Engine.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("pre-compressed asset headers = %v, want Content-Encoding gzip", w.Header())
	}
	if got := gunzip(t, w.Body.Bytes()); got != html {
		t.Fatalf("asset does not inflate to the page once: %d bytes", len(got))
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	s.Engine.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" || w.Body.String() != html {
		t.Fatalf("client without gzip got %v (%d bytes), want the inflated page", w.Header(), w.Body.Len())
	}
}

// A run's event stream reaches a gzip-accepting client event by event: the
// small run.interrupted a paused run ends on is not held back by the floor.
// Go's transport asks for gzip and inflates transparently, as a browser does.
func TestRunEventStreamIsNotGzipped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), staticAuth("tok"), nil)
	const event = "id: 1\nevent: run.interrupted\ndata: {\"run_id\":\"r1\"}\n\n"
	release := make(chan struct{})
	s.Engine.GET(APIPrefix+"/runs/:id/events", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Stream(func(w io.Writer) bool {
			_, _ = io.WriteString(w, event)
			return false
		})
		<-release // the run idles, paused for approval
	})
	srv := httptest.NewServer(s.Engine)
	defer srv.Close()
	defer close(release)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+APIPrefix+"/runs/r1/events", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the deferred call; the goroutine below only reads
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("event stream headers = %v, want no Content-Encoding", resp.Header)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case body := <-got:
		if body != event {
			t.Fatalf("first read = %q, want the event as written", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the event did not arrive while the run idled: held back by the gzip floor")
	}
}
