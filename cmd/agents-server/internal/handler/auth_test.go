package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// authEngine mounts the real middleware chain plus the auth routes, the way
// root.go wires them.
func authEngine(t *testing.T, svc *authn.Service) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := server.New(slog.New(slog.DiscardHandler), svc.Authenticate)
	s.RegisterAPI(Handlers{Auth: NewAuthHandler(svc)}.Register)
	return s.Engine
}

// Token mode: the login endpoint's error envelopes are part of the README
// "Errors" contract (these cases lived in server's envelope test before the
// auth routes moved here).
func TestTokenLoginEnvelopes(t *testing.T) {
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	engine := authEngine(t, authn.NewStatic("tok", local))

	cases := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"ok", `{"token":"tok"}`, http.StatusOK, `{"ok":true}`},
		{"wrong token", `{"token":"nope"}`, http.StatusUnauthorized,
			`{"error":{"code":"unauthorized","message":"invalid token"}}`},
		{"malformed body", `{`, http.StatusBadRequest,
			`{"error":{"code":"validation","message":"invalid request"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, engine, http.MethodPost, "/api/v1/auth/login", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tc.want {
				t.Errorf("body = %s, want %s", got, tc.want)
			}
		})
	}

	// config tells the login page it is in token mode.
	rec := doJSON(t, engine, http.MethodGet, "/api/v1/auth/config", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"token"`) {
		t.Fatalf("config = %d %s, want token mode", rec.Code, rec.Body.String())
	}
}

// OAuth mode end to end at the store level: a minted session token
// authenticates /auth/me and /auth/check, logout revokes it, and static token
// login is refused.
func TestOAuthModeSessionTokens(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users, tokens := store.NewUserStore(db), store.NewAuthTokenStore(db)
	u, err := users.ResolveOAuthLogin(ctx, store.OAuthIdentity{Provider: "google", Subject: "s1", Email: "a@example.com", Name: "A"}, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	secret, _, err := tokens.Mint(ctx, u.ID, store.TokenKindSession, "", time.Time{})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	engine := authEngine(t, authn.NewOAuth(users, tokens))

	bearer := func(r *http.Request) *http.Request {
		r.Header.Set("Authorization", "Bearer "+secret)
		return r
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, bearer(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"a@example.com"`) {
		t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, engine, http.MethodPost, "/api/v1/auth/login", `{"token":"whatever"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("token login in oauth mode = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, bearer(httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", rec.Code)
	}
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, bearer(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d, want 401", rec.Code)
	}
}

// The mounted auth group carries the per-IP rate limit.
func TestAuthGroupRateLimited(t *testing.T) {
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	engine := authEngine(t, authn.NewStatic("tok", local))

	last := 0
	for i := 0; i < 20; i++ {
		rec := doJSON(t, engine, http.MethodPost, "/api/v1/auth/login", `{"token":"nope"}`)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after hammering login = %d, want 429", last)
	}
}
