package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// tokenPayload is the JSON persisted for an OAuth grant: the oauth2.Token
// fields plus the refresh context — token endpoint and client credentials —
// needed to rebuild a REFRESHING token source after a restart. For a
// dynamically-registered client the credentials exist nowhere else. The
// client secret shares the column's sensitivity with the refresh token it
// sits next to (encryption at rest is a separately-tracked decision).
type tokenPayload struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`

	// Refresh context, absent in payloads written before the go-sdk v1.7.0
	// upgrade; such legacy grants degrade to a static token (usable until
	// expiry, then interactive re-authorization).
	TokenURL     string   `json:"token_url,omitempty"`
	AuthStyle    int      `json:"auth_style,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// persistGrant writes the grant through the store. Failures are logged, not
// returned: the in-memory token still works for this session, but a lost
// write means re-authorization after a restart — and store.ErrNotFound means
// the server row is gone. Loud, not fatal.
//
// ctx supplies the logger; the write itself is detached from its cancellation,
// because a refresh triggered by a request that then went away must still land.
func persistGrant(ctx context.Context, s *store.McpServerStore, configID string, ocfg *oauth2.Config, tok *oauth2.Token) {
	ctx = context.WithoutCancel(ctx)
	log := zerolog.Ctx(ctx)
	p := tokenPayload{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
		TokenURL:     ocfg.Endpoint.TokenURL,
		AuthStyle:    int(ocfg.Endpoint.AuthStyle),
		ClientID:     ocfg.ClientID,
		ClientSecret: ocfg.ClientSecret,
		Scopes:       ocfg.Scopes,
	}
	b, err := json.Marshal(p)
	if err != nil {
		log.Error().Err(err).Str("mcp", configID).Msg("encoding MCP OAuth grant failed")
		return
	}
	if err := s.SaveOAuthToken(ctx, configID, string(b)); err != nil {
		log.Error().Err(err).Str("mcp", configID).
			Msg("persisting MCP OAuth grant failed; connection works now but won't survive a restart")
	}
}

// persistingTokenSource wraps a refreshing oauth2.TokenSource and re-persists
// the grant whenever a refresh produced a new token. Without it a mid-session
// refresh — and any ROTATED refresh token — never reaches the store, so the
// next restart would resume from a stale, possibly revoked grant.
type persistingTokenSource struct {
	// ctx is what a refresh persists under. oauth2.TokenSource takes no
	// context, so it is captured here — detached from the caller's
	// cancellation (the source outlives any single request) and carrying the
	// configured logger.
	ctx      context.Context
	inner    oauth2.TokenSource
	cfg      *oauth2.Config
	configID string
	store    *store.McpServerStore

	mu   sync.Mutex
	last *oauth2.Token // last persisted token; unchanged tokens are not re-written
}

// newPersistingSource wraps inner. last is the token already persisted (the
// restored or just-persisted one), so serving it unchanged writes nothing.
func newPersistingSource(ctx context.Context, inner oauth2.TokenSource, ocfg *oauth2.Config, configID string, s *store.McpServerStore, last *oauth2.Token) *persistingTokenSource {
	return &persistingTokenSource{
		ctx:      context.WithoutCancel(ctx),
		inner:    inner,
		cfg:      ocfg,
		configID: configID,
		store:    s,
		last:     last,
	}
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil || tok == nil {
		return tok, err
	}
	p.mu.Lock()
	changed := p.last == nil || tok.AccessToken != p.last.AccessToken ||
		tok.RefreshToken != p.last.RefreshToken
	if changed {
		p.last = tok
	}
	p.mu.Unlock()
	// Persist outside the lock; concurrent callers of the same refreshed token
	// see changed=false while the first caller writes. (inner is a
	// ReuseTokenSource, which already serializes the refresh itself.)
	if changed {
		persistGrant(p.ctx, p.store, p.configID, p.cfg, tok)
	}
	return tok, nil
}

// restoredTokenSource rebuilds a token source from a persisted grant:
//
//   - full grant: a refreshing source that re-persists on change — the SAME
//     machinery a live authorization uses (NewTokenSource in
//     ConnectWithOAuth), so restored and fresh connections refresh alike and
//     an expired access token with a live refresh token reconnects silently.
//   - legacy or refresh-less grant with a still-valid access token: a static
//     source — usable until expiry, then interactive re-authorization.
//   - anything else: nil; the caller runs the interactive flow.
//
// hc is the HTTP client refreshes must use (proxy-aware, bounded timeout).
func restoredTokenSource(ctx context.Context, configID, saved string, s *store.McpServerStore, hc *http.Client) oauth2.TokenSource {
	if saved == "" {
		return nil
	}
	var p tokenPayload
	if err := json.Unmarshal([]byte(saved), &p); err != nil {
		return nil
	}
	tok := &oauth2.Token{
		AccessToken:  p.AccessToken,
		TokenType:    p.TokenType,
		RefreshToken: p.RefreshToken,
		Expiry:       p.Expiry,
	}
	if p.TokenURL == "" || p.ClientID == "" || p.RefreshToken == "" {
		if tok.Valid() {
			return oauth2.StaticTokenSource(tok)
		}
		return nil
	}
	ocfg := &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Endpoint: oauth2.Endpoint{
			TokenURL:  p.TokenURL,
			AuthStyle: oauth2.AuthStyle(p.AuthStyle),
		},
		Scopes: p.Scopes,
	}
	// Mirror the SDK's refresh context: detached from the caller's cancellation
	// — the source outlives any single request — carrying the HTTP client
	// oauth2 should use.
	rctx := context.WithValue(context.WithoutCancel(ctx), oauth2.HTTPClient, hc)
	return newPersistingSource(rctx, ocfg.TokenSource(rctx, tok), ocfg, configID, s, tok)
}
