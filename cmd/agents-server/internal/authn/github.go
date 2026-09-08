package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// GitHub is the GitHub OAuth login. The REST API supplies the identity after
// the code exchange: /user for the stable numeric id and the profile,
// /user/emails for the primary verified address, which /user omits when the
// person keeps it private.
type GitHub struct {
	ClientID     string
	ClientSecret string

	// Endpoint overrides for tests; empty means GitHub's.
	authURL  string
	tokenURL string
	apiURL   string
}

// Name implements OAuthProvider.
func (g *GitHub) Name() string { return "github" }

// AvatarHosts implements OAuthProvider: GitHub serves pictures from
// avatars.githubusercontent.com.
func (g *GitHub) AvatarHosts() []string { return []string{"https://avatars.githubusercontent.com"} }

func (g *GitHub) config(redirectURI string) *oauth2.Config {
	authURL := g.authURL
	if authURL == "" {
		authURL = "https://github.com/login/oauth/authorize"
	}
	tokenURL := g.tokenURL
	if tokenURL == "" {
		tokenURL = "https://github.com/login/oauth/access_token"
	}
	return &oauth2.Config{
		ClientID:     g.ClientID,
		ClientSecret: g.ClientSecret,
		RedirectURL:  redirectURI,
		// user:email reads the addresses; the profile fields are public.
		Scopes: []string{"user:email"},
		// GitHub documents the client credentials as POST parameters; naming
		// the style spares the auto-detect's second exchange on a refusal.
		Endpoint: oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
}

// AuthCodeURL implements OAuthProvider (PKCE S256).
func (g *GitHub) AuthCodeURL(state, verifier, redirectURI string) string {
	return g.config(redirectURI).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// Identity implements OAuthProvider: exchange the code, then read the profile
// and the addresses. The primary address must be verified; a verified
// secondary does not stand in for it.
func (g *GitHub) Identity(ctx context.Context, code, verifier, redirectURI string) (store.OAuthIdentity, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	tok, err := g.config(redirectURI).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return store.OAuthIdentity{}, fmt.Errorf("exchanging the code: %w", err)
	}

	var user struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := g.get(ctx, client, tok.AccessToken, "/user", &user); err != nil {
		return store.OAuthIdentity{}, err
	}
	if user.ID == 0 {
		return store.OAuthIdentity{}, errors.New("the profile carried no id")
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := g.get(ctx, client, tok.AccessToken, "/user/emails", &emails); err != nil {
		return store.OAuthIdentity{}, err
	}
	var email string
	for _, e := range emails {
		if e.Primary && e.Verified {
			email = e.Email
			break
		}
	}
	if email == "" {
		return store.OAuthIdentity{}, errors.New("the account's primary email is not verified")
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	return store.OAuthIdentity{
		Provider: g.Name(), Subject: strconv.FormatInt(user.ID, 10),
		Email: email, Name: name, AvatarURL: user.AvatarURL,
	}, nil
}

// get reads one REST resource as the signed-in person into out.
func (g *GitHub) get(ctx context.Context, client *http.Client, accessToken, path string, out any) error {
	apiURL := g.apiURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "agents-server")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(readCapped(resp.Body), out); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}
