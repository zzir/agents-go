package server

import (
	"net/url"
	"strings"
	"testing"
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
