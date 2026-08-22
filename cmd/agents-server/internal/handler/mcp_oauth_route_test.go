package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// The authorization server sends the browser back to mcpOAuthCallbackPath, and
// a redirect carries no Authorization header: that path must be a mounted route
// AND exempt from the token middleware, or every MCP OAuth login ends in 401.
func TestOAuthRedirectPathIsMountedAndUnauthenticated(t *testing.T) {
	path := server.APIPrefix + mcpOAuthCallbackPath

	gin.SetMode(gin.TestMode)

	mounted := newTestEngine()
	Handlers{}.Register(mounted.Group(server.APIPrefix))
	var found bool
	for _, r := range mounted.Routes() {
		if r.Method == http.MethodGet && r.Path == path {
			found = true
		}
	}
	if !found {
		t.Errorf("no GET route mounted at the OAuth redirect path %s", path)
	}

	// The exemption is checked on its own: the real callback handler needs a
	// wired McpServerHandler, and what matters here is that the middleware lets
	// the request through at all.
	guarded := gin.New()
	guarded.Use(server.TokenAuth(testAuthFunc("secret"), nil))
	guarded.GET(path, func(c *gin.Context) { c.Status(http.StatusOK) })
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET %s without a token = %d, want 200 (path must be auth-exempt)", path, rec.Code)
	}
}
