package authn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// fakeGitHubIDP stands in for GitHub's token endpoint and REST API in the
// shapes GitHub actually answers: a form-encoded token body (its default), a
// refusal as a 200 carrying error=, a profile whose email is null (kept
// private), and the addresses behind /user/emails. name is the profile's
// display name, nil for none.
func fakeGitHubIDP(t *testing.T, name any, emails []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token form: %v", err)
		}
		for _, k := range []string{"client_id", "client_secret", "code_verifier"} {
			if r.PostForm.Get(k) == "" {
				t.Errorf("token exchange lacks %s", k)
			}
		}
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		if r.PostForm.Get("code") != "code-1" {
			_, _ = w.Write([]byte("error=bad_verification_code&error_description=The+code+passed+is+incorrect+or+expired."))
			return
		}
		_, _ = w.Write([]byte("access_token=gho_1&scope=user%3Aemail&token_type=bearer"))
	})
	api := func(body any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gho_1" {
				t.Errorf("api auth = %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("User-Agent") == "" || r.Header.Get("Accept") != "application/vnd.github+json" {
				t.Errorf("api headers: User-Agent %q Accept %q", r.Header.Get("User-Agent"), r.Header.Get("Accept"))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		}
	}
	mux.HandleFunc("/user", api(map[string]any{
		"id": 42, "login": "octocat", "name": name, "email": nil,
		"avatar_url": "https://avatars.githubusercontent.com/u/42?v=4",
	}))
	mux.HandleFunc("/user/emails", api(emails))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testGitHub(idp *httptest.Server) *GitHub {
	return &GitHub{
		ClientID: "cid", ClientSecret: "csecret",
		authURL:  idp.URL + "/login/oauth/authorize",
		tokenURL: idp.URL + "/login/oauth/access_token",
		apiURL:   idp.URL,
	}
}

// verifiedPrimary is a private-email account's list: the noreply alias plus
// the real, primary, verified address.
var verifiedPrimary = []map[string]any{
	{"email": "42+octocat@users.noreply.github.com", "primary": false, "verified": true},
	{"email": "Octo@Example.com", "primary": true, "verified": true},
}

func TestGitHubIdentityRoundTrip(t *testing.T) {
	g := testGitHub(fakeGitHubIDP(t, "The Octocat", verifiedPrimary))
	id, err := g.Identity(context.Background(), "code-1", "verifier-1", "http://app.local/cb")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	want := store.OAuthIdentity{
		Provider: "github", Subject: "42", Email: "Octo@Example.com", Name: "The Octocat",
		AvatarURL: "https://avatars.githubusercontent.com/u/42?v=4",
	}
	if id != want {
		t.Fatalf("identity = %+v, want %+v", id, want)
	}
}

func TestGitHubNamesANamelessAccountByLogin(t *testing.T) {
	g := testGitHub(fakeGitHubIDP(t, nil, verifiedPrimary))
	id, err := g.Identity(context.Background(), "code-1", "v", "http://app.local/cb")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id.Name != "octocat" {
		t.Fatalf("name = %q, want the login", id.Name)
	}
}

// An unverified primary is refused; a verified secondary does not stand in.
func TestGitHubRejectsUnverifiedPrimaryEmail(t *testing.T) {
	emails := []map[string]any{
		{"email": "octo@example.com", "primary": true, "verified": false},
		{"email": "other@example.com", "primary": false, "verified": true},
	}
	g := testGitHub(fakeGitHubIDP(t, "x", emails))
	if _, err := g.Identity(context.Background(), "code-1", "v", "http://app.local/cb"); err == nil {
		t.Fatal("an unverified primary email must be refused")
	}
}

// GitHub refuses a code with a 200 carrying error=; that is a failed
// exchange, not a token with an empty access_token.
func TestGitHubRejectsABadCode(t *testing.T) {
	g := testGitHub(fakeGitHubIDP(t, "x", verifiedPrimary))
	if _, err := g.Identity(context.Background(), "code-9", "v", "http://app.local/cb"); err == nil {
		t.Fatal("a refused code must fail the exchange")
	}
}

// The login flow through the github provider: the authorize URL asks for the
// email scope with PKCE and lands on the provider's own callback, and the
// login completes to a session that authenticates. With both providers
// configured the login page and the CSP hear both.
func TestOAuthLoginFlowGitHub(t *testing.T) {
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
		BaseURL: "http://app.local",
		Providers: []OAuthProvider{
			testGoogle(fakeGoogleIDP(t, true)),
			testGitHub(fakeGitHubIDP(t, "The Octocat", verifiedPrimary)),
		},
		AllowedEmails: []string{"octo@example.com"},
	})

	authURL, nonce, err := svc.Begin("github")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("auth url: %v", err)
	}
	q := parsed.Query()
	if q.Get("scope") != "user:email" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		t.Fatalf("authorize URL = %s", authURL)
	}
	if got := q.Get("redirect_uri"); got != "http://app.local/api/v1/auth/oauth/github/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
	redirect := svc.Complete(ctx, "github", q.Get("state"), "code-1", nonce, "")
	code, ok := strings.CutPrefix(redirect, "http://app.local/#auth_code=")
	if !ok {
		t.Fatalf("callback redirect = %q", redirect)
	}
	token, user, ok := svc.Exchange(code)
	if !ok || user.Email != "octo@example.com" || user.Name != "The Octocat" || user.AvatarURL == "" {
		t.Fatalf("exchange = ok %v user %+v", ok, user)
	}
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("the minted session must authenticate: %v", err)
	}

	if got := svc.ConfigView().Providers; !slices.Equal(got, []string{"google", "github"}) {
		t.Fatalf("providers = %v", got)
	}
	want := []string{"https://*.googleusercontent.com", "https://avatars.githubusercontent.com"}
	if got := svc.AvatarHosts(); !slices.Equal(got, want) {
		t.Fatalf("avatar hosts = %v, want %v", got, want)
	}
}
