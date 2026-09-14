package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// ErrUnauthorized is an AuthFunc's answer to a credential that is wrong,
// expired or revoked — which of those, the transport never learns.
var ErrUnauthorized = errors.New("unauthorized")

// AuthFunc resolves a presented bearer credential to the calling user,
// ErrUnauthorized when it is not one, or another error when it cannot tell
// (the store is down) — refused 503 without charging the guess budget.
type AuthFunc func(ctx context.Context, bearer string) (protocol.UserInfo, error)

// GenerateToken returns a cryptographically random 32-character hex string.
func GenerateToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// BearerToken extracts the Authorization bearer credential, "" when absent.
// Query-string tokens are not accepted — they end up in browser history and
// proxy logs.
func BearerToken(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return h[7:]
	}
	return ""
}

// userContextKey is where TokenAuth parks the caller for CurrentUser.
const userContextKey = "agents.user"

// SetCurrentUser attaches the caller for CurrentUser — TokenAuth's half; tests
// that mount handlers without the middleware use it to sign a caller in.
func SetCurrentUser(c *gin.Context, u protocol.UserInfo) { c.Set(userContextKey, u) }

// CurrentUser returns the authenticated caller TokenAuth attached. The bool is
// false only on auth-exempt routes, where nobody authenticated.
func CurrentUser(c *gin.Context) (protocol.UserInfo, bool) {
	v, ok := c.Get(userContextKey)
	if !ok {
		return protocol.UserInfo{}, false
	}
	u, ok := v.(protocol.UserInfo)
	return u, ok
}

// authExempt reports whether path is reachable without a credential: the login
// page's needs, the OAuth callback, the OpenAPI document. Entries must be real routes.
func authExempt(path string) bool {
	switch {
	case path == APIPrefix+"/auth/login",
		path == APIPrefix+"/auth/config",
		path == APIPrefix+"/auth/exchange",
		strings.HasPrefix(path, APIPrefix+"/auth/oauth/"),
		path == APIPrefix+"/mcp-servers/oauth/callback",
		path == APIPrefix+"/openapi.yaml":
		return true
	}
	return false
}

// TokenAuth returns a gin middleware that authenticates /api/* requests via
// auth and attaches the caller for CurrentUser. /ws uses application-level
// auth (first WS message) against the same AuthFunc and guard. Failures
// draw on guard's per-IP budget; an exhausted IP answers 429 unchecked.
func TokenAuth(auth AuthFunc, guard *AuthGuard) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if authExempt(path) || !strings.HasPrefix(path, APIPrefix+"/") {
			c.Next()
			return
		}
		ip := c.ClientIP()
		if guard.Exhausted(ip) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				protocol.NewErrorResponse(protocol.CodeRateLimited, "too many failed credentials; slow down"))
			return
		}
		user, err := auth(c.Request.Context(), BearerToken(c))
		if err != nil {
			if !errors.Is(err, ErrUnauthorized) {
				logging.Ctx(c.Request.Context()).Error("credential check", "error", err)
				c.AbortWithStatusJSON(http.StatusServiceUnavailable,
					protocol.NewErrorResponse(protocol.CodeUnavailable, "credential could not be checked; retry"))
				return
			}
			guard.Failed(ip)
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				protocol.NewErrorResponse(protocol.CodeUnauthorized, "unauthorized"))
			return
		}
		SetCurrentUser(c, user)
		c.Next()
	}
}
