package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

// request logging must scrub the auth token AND the OAuth authorization
// code/state that ride the callback redirect (a leaked code is a usable
// credential), while leaving benign params intact.
func TestRedactQueryScrubsOAuthParams(t *testing.T) {
	u, err := url.Parse("/mcp-servers/oauth/callback?code=authcode123&state=st456&token=tok789&keep=ok")
	if err != nil {
		t.Fatal(err)
	}
	got := redactQuery(u)
	for _, leaked := range []string{"authcode123", "st456", "tok789"} {
		if strings.Contains(got, leaked) {
			t.Errorf("redactQuery leaked %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, "keep=ok") {
		t.Errorf("redactQuery dropped a non-secret param: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("redactQuery produced no REDACTED marker: %q", got)
	}
}

// A query with no sensitive params is returned path-only when empty, and
// otherwise preserved.
func TestRedactQueryNoSecrets(t *testing.T) {
	u, _ := url.Parse("/health")
	if got := redactQuery(u); got != "/health" {
		t.Errorf("redactQuery(/health) = %q, want /health", got)
	}
}

// An absolute-form request line with no path ("GET http://host HTTP/1.1", as
// scanners and proxies send it) parses to an empty URL.Path. The SPA fallback
// serves it the index like "/", rather than slicing past the end of it.
func TestSPAFallbackServesEmptyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), staticAuth("tok"), nil)
	s.ServeStatic(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}})

	r := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	if r.URL.Path != "" {
		t.Fatalf("URL.Path = %q, want the empty path this test is about", r.URL.Path)
	}
	w := httptest.NewRecorder()
	s.Engine.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "<!doctype html>" {
		t.Fatalf("empty path = %d %q, want 200 and the index", w.Code, w.Body.String())
	}
}
