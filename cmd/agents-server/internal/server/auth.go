package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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
// check endpoints themselves, the browser-facing OAuth redirect callbacks
// (which cannot carry an Authorization header), and the OpenAPI document.
func authExempt(path string) bool {
	for _, p := range []string{"/api/v1", "/api"} {
		switch {
		case strings.HasPrefix(path, p+"/auth/"),
			path == p+"/mcp-servers/oauth/callback",
			path == p+"/chatgpt/oauth/callback",
			path == p+"/openapi.yaml":
			return true
		}
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
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"code": "unauthorized", "message": "unauthorized"},
			})
			return
		}
		c.Next()
	}
}
