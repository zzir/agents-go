// Package authn answers "who is this bearer" for both auth modes — the static
// --token of a single-user workbench, and database-backed session tokens and
// PATs once OAuth login is configured. Everything else consumes the one
// Authenticate func; this is the only place the two modes meet.
package authn

import (
	"context"
	"crypto/subtle"
	"errors"

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
	users  *store.UserStore
	tokens *store.AuthTokenStore
}

// NewStatic returns the --auth token mode service: one constant-time-compared
// credential, one implicit account.
func NewStatic(staticToken string, local *store.User) *Service {
	return &Service{mode: ModeToken, staticToken: staticToken, localUser: userInfoOf(local)}
}

// NewOAuth returns the --auth oauth mode service, resolving bearers against
// the auth_tokens table.
func NewOAuth(users *store.UserStore, tokens *store.AuthTokenStore) *Service {
	return &Service{mode: ModeOAuth, users: users, tokens: tokens}
}

func userInfoOf(u *store.User) protocol.UserInfo {
	return protocol.UserInfo{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role}
}

// Mode reports which mode this service runs in (ModeToken or ModeOAuth).
func (s *Service) Mode() string { return s.mode }

// ConfigView is the /auth/config payload the login page renders from.
func (s *Service) ConfigView() protocol.AuthConfig {
	return protocol.AuthConfig{Mode: s.mode}
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
