package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// Every exempt path must name a route this router actually serves. An
// exemption for a path nothing serves is worse than dead config: whatever gets
// mounted there later is unauthenticated without a line changing here. The
// ChatGPT OAuth flow is the standing example of a path that needs NO exemption:
// its redirect lands on localhost, never on this server, and the pasted code
// returns through the authenticated /providers/:id/chatgpt/complete route.
func TestAuthExemptCoversOnlyServedRoutes(t *testing.T) {
	for _, p := range []string{
		"/api/v1/auth/login",
		"/api/v1/auth/config",
		"/api/v1/auth/exchange",
		"/api/v1/auth/oauth/google/start",
		"/api/v1/auth/oauth/google/callback",
		"/api/v1/mcp-servers/oauth/callback",
		"/api/v1/openapi.yaml",
	} {
		if !authExempt(p) {
			t.Errorf("authExempt(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"/api/v1/chatgpt/oauth/callback",
		"/api/auth/login",
		"/api/v1/auth/check",
		"/api/v1/auth/me",
		"/api/v1/auth/logout",
		"/api/v1/auth/tokens",
		"/api/v1/sessions",
		"/api/v1/providers/p1/chatgpt/login",
	} {
		if authExempt(p) {
			t.Errorf("authExempt(%q) = true, want false", p)
		}
	}
}

// The non-2xx responses this package writes itself — the auth middleware and
// the JSON 404 for unmatched API paths — go out as the envelope documented in
// README "Errors". Compared against literal bytes rather than a re-marshalled
// protocol.ErrorResponse: the bytes are the contract, and a renamed field
// would move both sides of that comparison at once. handler/contract_test.go
// pins the same bytes from the other emitter; the login endpoint's live in
// handler/auth_test.go since the auth routes moved there.
func TestErrorEnvelopeMatchesTheSharedShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := New(slog.New(slog.DiscardHandler), staticAuth("tok"), nil)
	s.ServeStatic(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}})

	authed := func(r *http.Request) *http.Request {
		r.Header.Set("Authorization", "Bearer tok")
		return r
	}

	cases := []struct {
		name   string
		req    *http.Request
		status int
		body   string
	}{
		{"no token", httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil), http.StatusUnauthorized,
			`{"error":{"code":"unauthorized","message":"unauthorized"}}`},
		{"unmatched api path", authed(httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil)), http.StatusNotFound,
			`{"error":{"code":"not_found","message":"not found"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.Engine.ServeHTTP(w, tc.req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.status, w.Body.String())
			}
			if got := strings.TrimSpace(w.Body.String()); got != tc.body {
				t.Errorf("body = %s, want %s", got, tc.body)
			}
		})
	}
}

// A credential the store cannot resolve is refused 503 `unavailable`, not
// 401: the caller keeps its token and the guess budget is not charged, so an
// outage cannot sign an office out or lock its NAT address. A wrong
// credential is still 401 and, repeated, still exhausts the budget.
func TestUnresolvableCredentialIs503AndUncharged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var down atomic.Bool
	auth := func(ctx context.Context, bearer string) (protocol.UserInfo, error) {
		if down.Load() {
			return protocol.UserInfo{}, errors.New("sql: database is closed")
		}
		return staticAuth("tok")(ctx, bearer)
	}
	s := New(slog.New(slog.DiscardHandler), auth, nil)
	s.Engine.GET(APIPrefix+"/ping", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	do := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, APIPrefix+"/ping", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Engine.ServeHTTP(w, r)
		return w
	}

	down.Store(true)
	const want = `{"error":{"code":"unavailable","message":"credential could not be checked; retry"}}`
	for range authRateBurst + 5 {
		if w := do("tok"); w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != want {
			t.Fatalf("during the outage = %d %s, want 503 %s", w.Code, w.Body.String(), want)
		}
	}
	down.Store(false)
	if w := do("tok"); w.Code != http.StatusOK {
		t.Fatalf("after the outage = %d, want 200: the outage charged the guess budget", w.Code)
	}

	for range authRateBurst {
		if w := do("nope"); w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong credential = %d, want 401", w.Code)
		}
	}
	if w := do("nope"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("wrong credential past the budget = %d, want 429", w.Code)
	}
}
