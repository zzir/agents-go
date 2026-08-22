package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
func authEngine(t *testing.T, svc *authn.Service, tokens *store.AuthTokenStore) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := server.New(slog.New(slog.DiscardHandler), svc.Authenticate, nil)
	s.RegisterAPI(Handlers{Auth: NewAuthHandler(svc, tokens, nil, nil)}.Register)
	return s.Engine
}

// Token mode: the login endpoint's error envelopes are part of the README
// "Errors" contract (these cases lived in server's envelope test before the
// auth routes moved here).
func TestTokenLoginEnvelopes(t *testing.T) {
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	engine := authEngine(t, authn.NewStatic("tok", local), nil)

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
	engine := authEngine(t, authn.NewOAuth(authn.OAuthConfig{Users: users, Tokens: tokens}), tokens)

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

// fakeLoginProvider skips the external IdP: any code yields a fixed identity. What
// this test exercises is the HTTP surface — routing, auth exemption, the
// redirect contract, and the one-time exchange.
type fakeLoginProvider struct{ email string }

func (f *fakeLoginProvider) Name() string          { return "fake" }
func (f *fakeLoginProvider) AvatarHosts() []string { return nil }
func (f *fakeLoginProvider) AuthCodeURL(state, _, redirectURI string) string {
	return "https://idp.example/authorize?state=" + state + "&redirect_uri=" + redirectURI
}
func (f *fakeLoginProvider) Identity(_ context.Context, code, _, _ string) (store.OAuthIdentity, error) {
	return store.OAuthIdentity{Provider: "fake", Subject: "s-" + code, Email: f.email, Name: "F"}, nil
}

// The full login flow over HTTP: start redirects out with state, the callback
// redirects into the SPA with a one-time code, exchange yields the session
// token, and that token opens /auth/me.
func TestOAuthFlowOverHTTP(t *testing.T) {
	db := newTestDB(t)
	svc := authn.NewOAuth(authn.OAuthConfig{
		Users: store.NewUserStore(db), Tokens: store.NewAuthTokenStore(db),
		BaseURL:       "http://app.local",
		Providers:     []authn.OAuthProvider{&fakeLoginProvider{email: "p@example.com"}},
		AllowedEmails: []string{"p@example.com"},
	})
	engine := authEngine(t, svc, nil)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/fake/start", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Query().Get("state") == "" {
		t.Fatalf("start location = %q (%v)", rec.Header().Get("Location"), err)
	}
	// The browser's half of the login rides a cookie: HttpOnly, Lax (the
	// provider's redirect back is a top-level navigation), not Secure on a
	// plain-http base URL.
	var loginCookie *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "agents_oauth" {
			loginCookie = ck
		}
	}
	if loginCookie == nil || loginCookie.Value == "" || !loginCookie.HttpOnly || loginCookie.SameSite != http.SameSiteLaxMode || loginCookie.Path != "/" {
		t.Fatalf("start must set the login cookie (HttpOnly, Lax, path /): %+v", loginCookie)
	}

	// A callback from a browser that did not start the login — no cookie —
	// is a mismatch, and spends the state: the login CSRF that would land a
	// victim inside the attacker's account.
	callback := "/api/v1/auth/oauth/fake/callback?state=" + loc.Query().Get("state") + "&code=c1"
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, callback, nil))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "auth_error=state_mismatch") {
		t.Fatalf("callback without the cookie = %d %q, want state_mismatch", rec.Code, rec.Header().Get("Location"))
	}

	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/fake/start", nil))
	loc, _ = url.Parse(rec.Header().Get("Location"))
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "agents_oauth" {
			loginCookie = ck
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/fake/callback?state="+loc.Query().Get("state")+"&code=c1", nil)
	req.AddCookie(loginCookie)
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302", rec.Code)
	}
	code, ok := strings.CutPrefix(rec.Header().Get("Location"), "http://app.local/#auth_code=")
	if !ok {
		t.Fatalf("callback location = %q", rec.Header().Get("Location"))
	}
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "agents_oauth" && ck.MaxAge >= 0 {
			t.Fatalf("the callback must clear the login cookie, got %+v", ck)
		}
	}

	rec = doJSON(t, engine, http.MethodPost, "/api/v1/auth/exchange", `{"code":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange = %d %s", rec.Code, rec.Body.String())
	}
	var session struct {
		Token string `json:"token"`
		User  struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil || session.Token == "" {
		t.Fatalf("exchange body %s (%v)", rec.Body.String(), err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"p@example.com"`) {
		t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
	}

	// An unknown provider 404s; a replayed exchange code 401s.
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/nope/start", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider start = %d, want 404", rec.Code)
	}
	rec = doJSON(t, engine, http.MethodPost, "/api/v1/auth/exchange", `{"code":"`+code+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("exchange replay = %d, want 401", rec.Code)
	}
}

// PATs: minted once with the plaintext in that response alone, listed without
// secrets, usable as a bearer, revocable — and refused outright in token mode,
// where they could never authenticate.
func TestPersonalAccessTokens(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	users, tokens := store.NewUserStore(db), store.NewAuthTokenStore(db)
	u, err := users.ResolveOAuthLogin(ctx, store.OAuthIdentity{Provider: "google", Subject: "s1", Email: "a@example.com"}, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	session, _, err := tokens.Mint(ctx, u.ID, store.TokenKindSession, "", time.Time{})
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	engine := authEngine(t, authn.NewOAuth(authn.OAuthConfig{Users: users, Tokens: tokens}), tokens)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var rdr *strings.Reader
		if body == "" {
			rdr = strings.NewReader("")
		} else {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Authorization", "Bearer "+session)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"ci"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Token string `json:"token"`
		Pat   struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"pat"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil ||
		!strings.HasPrefix(created.Token, "ags_p_") || created.Pat.Name != "ci" {
		t.Fatalf("create body %s (%v)", rec.Body.String(), err)
	}

	// The PAT authenticates like any bearer.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me with PAT = %d", rec.Code)
	}

	// The list carries names and dates, never token material.
	rec = do(http.MethodGet, "/api/v1/auth/tokens", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "ags_p_") ||
		!strings.Contains(rec.Body.String(), `"ci"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	// A nameless token is refused — a label is what makes revocation usable.
	if rec = do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("nameless create = %d, want 400", rec.Code)
	}

	rec = do(http.MethodDelete, "/api/v1/auth/tokens/"+created.Pat.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me with revoked PAT = %d, want 401", rec.Code)
	}

	// Token mode refuses the whole PAT surface.
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	tokenEngine := authEngine(t, authn.NewStatic("tok", local), tokens)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/tokens", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	tokenEngine.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATs in token mode = %d, want 400", rec.Code)
	}
}

// The mounted auth group carries the per-IP rate limit.
func TestAuthGroupRateLimited(t *testing.T) {
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	engine := authEngine(t, authn.NewStatic("tok", local), nil)

	last := 0
	for range 20 {
		rec := doJSON(t, engine, http.MethodPost, "/api/v1/auth/login", `{"token":"nope"}`)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after hammering login = %d, want 429", last)
	}
}

// A valid credential never spends budget: a signed-in caller hammering
// /auth/me or the SPA's /auth/check probe (every page load, every tab) must
// never see 429 — a 429 there signs a legitimate user out.
func TestValidCredentialIsNeverRateLimited(t *testing.T) {
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	engine := authEngine(t, authn.NewStatic("tok", local), nil)
	for _, path := range []string{"/api/v1/auth/me", "/api/v1/auth/check", "/api/v1/auth/config"} {
		for i := range 30 {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer tok")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("request %d to %s = %d, want 200", i+1, path, rec.Code)
			}
		}
	}
}

// Failed bearers draw on a per-IP budget on EVERY authenticated route, so a
// weak static token cannot be brute-forced at line rate through /auth/me;
// once exhausted the IP is refused unchecked, and a valid bearer from the
// same IP is refused too until the budget refills.
func TestFailedBearersExhaustTheGuessBudget(t *testing.T) {
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	engine := authEngine(t, authn.NewStatic("tok", local), nil)
	last := 0
	for range 20 {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.Header.Set("Authorization", "Bearer nope")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after 20 bad bearers = %d, want 429", last)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("valid bearer from an exhausted IP = %d, want 429", rec.Code)
	}
}

// The budget is per client IP: X-Forwarded-For is ignored unless the direct
// peer is a trusted proxy, so a direct client cannot dodge the budget by
// rotating the header, and behind a trusted proxy each forwarded client has
// its own budget instead of sharing the proxy's.
func TestGuessBudgetKeyedByClientIP(t *testing.T) {
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	bad := func(engine *gin.Engine, forwarded string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer nope")
		req.Header.Set("X-Forwarded-For", forwarded)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec.Code
	}

	untrusted := authEngine(t, authn.NewStatic("tok", local), nil)
	last := 0
	for i := range 20 {
		last = bad(untrusted, "203.0.113."+strconv.Itoa(i))
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("rotating X-Forwarded-For from an untrusted peer dodged the budget: %d", last)
	}

	gin.SetMode(gin.TestMode)
	s := server.New(slog.New(slog.DiscardHandler), authn.NewStatic("tok", local).Authenticate, nil)
	if err := s.SetTrustedProxies([]string{"10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	s.RegisterAPI(Handlers{Auth: NewAuthHandler(authn.NewStatic("tok", local), nil, nil, nil)}.Register)
	for range 20 {
		bad(s.Engine, "203.0.113.7")
	}
	if got := bad(s.Engine, "203.0.113.8"); got != http.StatusUnauthorized {
		t.Fatalf("behind a trusted proxy a different forwarded client = %d, want 401 (own budget)", got)
	}
}
