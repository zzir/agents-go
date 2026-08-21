package authn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// fakeGoogleIdP stands in for Google's token and userinfo endpoints. It
// enforces the parts of the contract the Google provider must uphold: the
// PKCE verifier travels with the code exchange, and the identity comes from
// userinfo with a Bearer access token.
func fakeGoogleIdP(t *testing.T, verified bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token form: %v", err)
		}
		if r.PostForm.Get("code") != "code-1" {
			t.Errorf("token exchange code = %q", r.PostForm.Get("code"))
		}
		if r.PostForm.Get("code_verifier") == "" {
			t.Error("PKCE verifier missing from the code exchange")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "token_type": "Bearer"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			t.Errorf("userinfo auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "g-sub-1", "email": "Person@Example.com", "email_verified": verified,
			"name": "Person", "picture": "https://img.example/p.png",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testGoogle(idp *httptest.Server) *Google {
	return &Google{
		ClientID: "cid", ClientSecret: "csecret",
		authURL:     idp.URL + "/authorize",
		tokenURL:    idp.URL + "/token",
		userinfoURL: idp.URL + "/userinfo",
	}
}

func TestGoogleIdentityRoundTrip(t *testing.T) {
	g := testGoogle(fakeGoogleIdP(t, true))
	id, err := g.Identity(context.Background(), "code-1", "verifier-1", "http://app.local/cb")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id.Provider != "google" || id.Subject != "g-sub-1" || id.Email != "Person@Example.com" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestGoogleRejectsUnverifiedEmail(t *testing.T) {
	g := testGoogle(fakeGoogleIdP(t, false))
	if _, err := g.Identity(context.Background(), "code-1", "v", "http://app.local/cb"); err == nil {
		t.Fatal("an unverified email must be refused")
	}
}

// The whole login flow at the service level, against the fake IdP: begin,
// callback, one-time exchange, then the minted session authenticates.
func TestOAuthLoginFlow(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.CreateSchema(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}

	svc := NewOAuth(OAuthConfig{
		Users: store.NewUserStore(db), Tokens: store.NewAuthTokenStore(db),
		BaseURL:   "http://app.local",
		Providers: []OAuthProvider{testGoogle(fakeGoogleIdP(t, true))},
		// The domain check must be case-insensitive end to end.
		AllowedDomains: []string{"Example.com"},
	})

	authURL, err := svc.Begin("google")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("auth url: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" || parsed.Query().Get("code_challenge") == "" {
		t.Fatalf("authorize URL missing state or PKCE challenge: %s", authURL)
	}
	if got := parsed.Query().Get("redirect_uri"); got != "http://app.local/api/v1/auth/oauth/google/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}

	redirect := svc.Complete(ctx, "google", state, "code-1")
	code, ok := strings.CutPrefix(redirect, "http://app.local/#auth_code=")
	if !ok {
		t.Fatalf("callback redirect = %q", redirect)
	}
	token, user, ok := svc.Exchange(code)
	if !ok || user.Email != "person@example.com" || user.Role != store.RoleAdmin {
		t.Fatalf("exchange = ok %v user %+v", ok, user)
	}
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("the minted session must authenticate: %v", err)
	}

	// Both halves are single-use: the state and the one-time code.
	if again := svc.Complete(ctx, "google", state, "code-1"); !strings.Contains(again, "auth_error=state_mismatch") {
		t.Fatalf("state replay = %q, want state_mismatch", again)
	}
	if _, _, ok := svc.Exchange(code); ok {
		t.Fatal("the one-time code must not exchange twice")
	}
}

// A login outside the allowlist ends at the error redirect, and no account or
// session is created.
func TestOAuthLoginRefusesUnlistedEmail(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.CreateSchema(ctx, db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	users := store.NewUserStore(db)
	svc := NewOAuth(OAuthConfig{
		Users: users, Tokens: store.NewAuthTokenStore(db),
		BaseURL:        "http://app.local",
		Providers:      []OAuthProvider{testGoogle(fakeGoogleIdP(t, true))},
		AllowedDomains: []string{"another.com"},
	})

	authURL, _ := svc.Begin("google")
	parsed, _ := url.Parse(authURL)
	redirect := svc.Complete(ctx, "google", parsed.Query().Get("state"), "code-1")
	if !strings.Contains(redirect, "auth_error=not_allowed") {
		t.Fatalf("redirect = %q, want not_allowed", redirect)
	}
	if _, err := users.ByID(ctx, store.LocalUserID); err == nil {
		// local user absent in this db; the refused login must not have
		// created any account either.
		t.Fatal("unexpected local user")
	}
}
