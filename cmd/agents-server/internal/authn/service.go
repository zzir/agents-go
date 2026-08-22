// Package authn answers "who is this bearer" for both auth modes — the static
// --token of a single-user workbench, and database-backed session tokens and
// PATs once OAuth login is configured. Everything else consumes the one
// Authenticate func; this is the only place the two modes meet.
package authn

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Auth modes, the AuthConfig.Mode vocabulary.
const (
	ModeToken = "token"
	ModeOAuth = "oauth"
)

// ErrUnauthorized is every authentication failure: wrong, expired, and revoked
// are indistinguishable to the caller on purpose.
var ErrUnauthorized = errors.New("unauthorized")

// Service resolves bearer credentials and (in OAuth mode) runs login flows.
type Service struct {
	mode string

	// Token mode: the static credential and the one account it maps to.
	staticToken string
	localUser   protocol.UserInfo

	// OAuth mode.
	users          *store.UserStore
	tokens         *store.AuthTokenStore
	baseURL        string
	providers      map[string]OAuthProvider
	providerNames  []string // registration order, for ConfigView
	allowedDomains []string
	allowedEmails  []string
	bootstrapAdmin string
	log            *slog.Logger

	// In-flight login state, process-local (see loginTTL).
	mu       sync.Mutex
	logins   map[string]pendingLogin    // by state
	sessions map[string]pendingExchange // by one-time exchange code
}

// NewStatic returns the --auth token mode service: one constant-time-compared
// credential, one implicit account.
func NewStatic(staticToken string, local *store.User) *Service {
	return &Service{mode: ModeToken, staticToken: staticToken, localUser: userInfoOf(local)}
}

// OAuthConfig configures the --auth oauth mode service. Root validates the
// combination (base URL present, at least one provider, a non-empty allowlist)
// before construction.
type OAuthConfig struct {
	Users  *store.UserStore
	Tokens *store.AuthTokenStore
	// BaseURL is the public origin every redirect URI derives from.
	BaseURL   string
	Providers []OAuthProvider
	// AllowedDomains / AllowedEmails admit a verified email; BootstrapAdmin is
	// implicitly admitted and lands as admin.
	AllowedDomains []string
	AllowedEmails  []string
	BootstrapAdmin string
	Log            *slog.Logger
}

// NewOAuth returns the --auth oauth mode service, resolving bearers against
// the auth_tokens table and running the configured providers' login flows.
func NewOAuth(cfg OAuthConfig) *Service {
	s := &Service{
		mode:           ModeOAuth,
		users:          cfg.Users,
		tokens:         cfg.Tokens,
		baseURL:        cfg.BaseURL,
		providers:      make(map[string]OAuthProvider, len(cfg.Providers)),
		allowedDomains: lowerAll(cfg.AllowedDomains),
		allowedEmails:  lowerAll(cfg.AllowedEmails),
		bootstrapAdmin: strings.ToLower(strings.TrimSpace(cfg.BootstrapAdmin)),
		log:            cfg.Log,
		logins:         make(map[string]pendingLogin),
		sessions:       make(map[string]pendingExchange),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	for _, p := range cfg.Providers {
		s.providers[p.Name()] = p
		s.providerNames = append(s.providerNames, p.Name())
	}
	return s
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func userInfoOf(u *store.User) protocol.UserInfo {
	return protocol.UserInfo{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role}
}

// Mode reports which mode this service runs in (ModeToken or ModeOAuth).
func (s *Service) Mode() string { return s.mode }

// ConfigView is the /auth/config payload the login page renders from.
func (s *Service) ConfigView() protocol.AuthConfig {
	return protocol.AuthConfig{Mode: s.mode, Providers: s.providerNames}
}

// Authenticate resolves a presented bearer to its user, or ErrUnauthorized.
func (s *Service) Authenticate(ctx context.Context, bearer string) (protocol.UserInfo, error) {
	if bearer == "" {
		return protocol.UserInfo{}, ErrUnauthorized
	}
	if s.mode == ModeToken {
		if s.staticToken == "" || subtle.ConstantTimeCompare([]byte(bearer), []byte(s.staticToken)) != 1 {
			return protocol.UserInfo{}, ErrUnauthorized
		}
		return s.localUser, nil
	}
	u, _, err := s.tokens.Authenticate(ctx, bearer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.UserInfo{}, ErrUnauthorized
		}
		return protocol.UserInfo{}, err
	}
	return userInfoOf(u), nil
}

// StaticOK reports whether token is the static credential — the /auth/login
// check. Always false in OAuth mode, where token login is disabled.
func (s *Service) StaticOK(token string) bool {
	return s.mode == ModeToken && token != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(s.staticToken)) == 1
}

// Logout revokes the presented session token. A no-op in token mode — the
// static credential has nothing to revoke.
func (s *Service) Logout(ctx context.Context, bearer string) error {
	if s.mode == ModeToken || bearer == "" {
		return nil
	}
	if err := s.tokens.RevokeByPlaintext(ctx, bearer); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// redirectURI is where the provider sends the browser back — derived from the
// base URL, never from request headers.
func (s *Service) redirectURI(provider string) string {
	return s.baseURL + "/api/v1/auth/oauth/" + provider + "/callback"
}

// Begin starts one login: mints state + PKCE verifier, parks them, and returns
// the provider's authorize URL to redirect the browser to.
func (s *Service) Begin(provider string) (string, error) {
	p, ok := s.providers[provider]
	if !ok {
		return "", store.ErrNotFound
	}
	state, verifier := randomToken(), oauth2.GenerateVerifier()
	s.mu.Lock()
	s.pruneLocked(time.Now())
	s.logins[state] = pendingLogin{provider: provider, verifier: verifier, created: time.Now()}
	s.mu.Unlock()
	return p.AuthCodeURL(state, verifier, s.redirectURI(provider)), nil
}

// Complete finishes one login from the provider's callback and always returns
// a redirect for the browser: on success into the SPA carrying a one-time
// exchange code in the fragment (fragments stay out of logs, and the session
// token itself never rides a URL), on failure carrying a coarse error tag —
// the detail goes to the log, not to an unauthenticated visitor.
func (s *Service) Complete(ctx context.Context, provider, state, code string) string {
	fail := func(tag string, err error) string {
		s.log.Warn("oauth login failed", "provider", provider, "reason", tag, "error", err)
		return s.baseURL + "/#auth_error=" + url.QueryEscape(tag)
	}
	s.mu.Lock()
	pending, ok := s.logins[state]
	delete(s.logins, state) // single use, hit or miss
	s.mu.Unlock()
	if !ok || pending.provider != provider || time.Since(pending.created) > loginTTL || code == "" {
		return fail("state_mismatch", nil)
	}
	p := s.providers[provider]
	id, err := p.Identity(ctx, code, pending.verifier, s.redirectURI(provider))
	if err != nil {
		return fail("exchange_failed", err)
	}
	if !s.allowed(id.Email) {
		return fail("not_allowed", errors.New("email "+id.Email+" is not on the allowlist"))
	}
	u, err := s.users.ResolveOAuthLogin(ctx, id, s.bootstrapAdmin)
	if err != nil {
		return fail("login_failed", err)
	}
	token, _, err := s.tokens.Mint(ctx, u.ID, store.TokenKindSession, "", time.Time{})
	if err != nil {
		return fail("login_failed", err)
	}
	// The picture is fetched off the login path: a slow image host must not
	// delay the redirect, and a failed fetch costs only the picture.
	if id.AvatarURL != "" {
		go s.fetchAvatar(u.ID, id.AvatarURL)
	}
	oneTime := randomToken()
	s.mu.Lock()
	s.sessions[oneTime] = pendingExchange{token: token, user: *u, created: time.Now()}
	s.mu.Unlock()
	s.log.Info("oauth login", "provider", provider, "user", u.Email, "role", u.Role)
	return s.baseURL + "/#auth_code=" + oneTime
}

// Exchange trades the callback's one-time code for the minted session token.
// Single use, short-lived; a miss is indistinct from an expiry.
func (s *Service) Exchange(code string) (string, protocol.UserInfo, bool) {
	s.mu.Lock()
	pending, ok := s.sessions[code]
	delete(s.sessions, code)
	s.pruneLocked(time.Now())
	s.mu.Unlock()
	if !ok || time.Since(pending.created) > exchangeTTL {
		return "", protocol.UserInfo{}, false
	}
	return pending.token, userInfoOf(&pending.user), true
}

// pruneLocked drops expired in-flight login state; called under mu from the
// paths that grow the maps.
func (s *Service) pruneLocked(now time.Time) {
	for k, v := range s.logins {
		if now.Sub(v.created) > loginTTL {
			delete(s.logins, k)
		}
	}
	for k, v := range s.sessions {
		if now.Sub(v.created) > exchangeTTL {
			delete(s.sessions, k)
		}
	}
}

// allowed applies the admission lists to a verified email. The bootstrap
// admin is implicitly admitted, so the address is not configured twice.
func (s *Service) allowed(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	if email == s.bootstrapAdmin {
		return true
	}
	for _, e := range s.allowedEmails {
		if e == email {
			return true
		}
	}
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return false
	}
	for _, d := range s.allowedDomains {
		if d == domain {
			return true
		}
	}
	return false
}

// Avatar limits: the provider's picture is stored, not hot-linked, so the
// CSP stays at img-src 'self'. A bound keeps a hostile provider from filling
// the users table.
const (
	avatarMaxBytes = 256 << 10
	avatarTimeout  = 5 * time.Second
)

// fetchAvatar downloads the provider's picture into the user's row. Any
// failure is logged and forgotten — the Account panel falls back to initials.
func (s *Service) fetchAvatar(userID, avatarURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), avatarTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, avatarURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.log.Debug("avatar fetch failed", "user", userID, "error", err)
		return
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ctype, "image/") {
		s.log.Debug("avatar fetch refused", "user", userID, "status", resp.StatusCode, "content_type", ctype)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, avatarMaxBytes+1))
	if err != nil || len(data) > avatarMaxBytes {
		s.log.Debug("avatar too large or unreadable", "user", userID, "bytes", len(data))
		return
	}
	if err := s.users.SetAvatar(ctx, userID, data, ctype); err != nil {
		s.log.Warn("storing avatar failed", "user", userID, "error", err)
	}
}
