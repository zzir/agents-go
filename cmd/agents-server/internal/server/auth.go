package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// GenerateToken returns a cryptographically random 32-character hex string.
func GenerateToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func extractToken(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return h[7:]
	}
	return ""
}

// authExempt reports whether path is reachable without a token: the login /
// check endpoints themselves, the browser-facing OAuth redirect callback
// (which cannot carry an Authorization header), and the OpenAPI document.
//
// Every entry must name a route that exists — an exemption for a path nothing
// serves silently unauthenticates whatever gets mounted there later. The
// ChatGPT OAuth callback is not here for that reason: it is served by a
// temporary listener on localhost:1455, never by this router.
func authExempt(path string) bool {
	switch {
	case strings.HasPrefix(path, APIPrefix+"/auth/"),
		path == APIPrefix+"/mcp-servers/oauth/callback",
		path == APIPrefix+"/openapi.yaml":
		return true
	}
	return false
}

// TokenAuth returns a gin middleware that requires a valid Bearer token on
// /api/* paths (query-string tokens are not accepted — they end up in
// browser history and proxy logs). /ws uses application-level auth (first WS
// message).
func TokenAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if authExempt(path) || !strings.HasPrefix(path, "/api/") {
			c.Next()
			return
		}

		provided := extractToken(c)
		if token == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				protocol.NewErrorResponse(protocol.CodeUnauthorized, "unauthorized"))
			return
		}
		c.Next()
	}
}
