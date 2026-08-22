package authn

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// OAuthProvider runs one external login: build the authorize redirect, then
// turn the returned code into a verified identity.
type OAuthProvider interface {
	Name() string
	AuthCodeURL(state, verifier, redirectURI string) string
	Identity(ctx context.Context, code, verifier, redirectURI string) (store.OAuthIdentity, error)
	// AvatarHosts are the CSP img-src sources the provider's pictures load
	// from — the one hole the policy opens, per configured provider.
	AvatarHosts() []string
}

// Login-flow windows. Pending state is process-local by design (single
// instance); multi-instance moves it to the database.
const (
	// LoginTTL bounds an authorize round-trip: state older than this is gone,
	// and the browser's login cookie lives exactly as long.
	LoginTTL = 10 * time.Minute
	// exchangeTTL bounds the one-time code's life between the callback
	// redirect and the SPA's exchange call — one page load.
	exchangeTTL = time.Minute
)

// pendingLogin is one authorize round-trip in flight, keyed by state.
type pendingLogin struct {
	provider string
	verifier string
	// nonce is the browser's half: set in a cookie at start, it ties the
	// callback to the tab that began the login (a state alone is a secret the
	// attacker's own login hands them).
	nonce   string
	created time.Time
}

// pendingExchange is one minted session waiting for the SPA to collect it,
// keyed by the one-time code the callback redirect carries. The session token
// itself never appears in a URL.
type pendingExchange struct {
	token   string
	user    store.User
	created time.Time
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Google is the Google OIDC login. Identity comes from the userinfo endpoint
// rather than the id_token: the code exchange talks to Google directly over
// TLS, which is the source binding a signature check would re-prove, and
// skipping JWT parsing keeps the dependency out.
type Google struct {
	ClientID     string
	ClientSecret string

	// Endpoint overrides for tests; empty means Google's.
	authURL     string
	tokenURL    string
	userinfoURL string
}

// Name implements OAuthProvider.
func (g *Google) Name() string { return "google" }

// AvatarHosts implements OAuthProvider: Google serves pictures from
// *.googleusercontent.com.
func (g *Google) AvatarHosts() []string { return []string{"https://*.googleusercontent.com"} }

func (g *Google) config(redirectURI string) *oauth2.Config {
	authURL := g.authURL
	if authURL == "" {
		authURL = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	tokenURL := g.tokenURL
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	return &oauth2.Config{
		ClientID:     g.ClientID,
		ClientSecret: g.ClientSecret,
		RedirectURL:  redirectURI,
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint:     oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL},
	}
}

// AuthCodeURL implements OAuthProvider (PKCE S256).
func (g *Google) AuthCodeURL(state, verifier, redirectURI string) string {
	return g.config(redirectURI).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Identity implements OAuthProvider: exchange the code, then read userinfo.
func (g *Google) Identity(ctx context.Context, code, verifier, redirectURI string) (store.OAuthIdentity, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	tok, err := g.config(redirectURI).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return store.OAuthIdentity{}, fmt.Errorf("exchanging the code: %w", err)
	}

	userinfoURL := g.userinfoURL
	if userinfoURL == "" {
		userinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return store.OAuthIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := client.Do(req)
	if err != nil {
		return store.OAuthIdentity{}, fmt.Errorf("reading userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return store.OAuthIdentity{}, fmt.Errorf("userinfo answered %d", resp.StatusCode)
	}
	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := json.Unmarshal(readCapped(resp.Body), &info); err != nil {
		return store.OAuthIdentity{}, fmt.Errorf("decoding userinfo: %w", err)
	}
	if info.Sub == "" || info.Email == "" {
		return store.OAuthIdentity{}, errors.New("userinfo carried no subject or email")
	}
	if !info.EmailVerified {
		return store.OAuthIdentity{}, errors.New("the account's email is not verified")
	}
	return store.OAuthIdentity{
		Provider: g.Name(), Subject: info.Sub,
		Email: info.Email, Name: info.Name, AvatarURL: info.Picture,
	}, nil
}

func readCapped(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	return b
}
