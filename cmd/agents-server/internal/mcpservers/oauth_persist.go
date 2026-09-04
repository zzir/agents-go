package mcpservers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// tokenPayload is the JSON persisted for an OAuth grant: the oauth2.Token plus
// the refresh context needed to rebuild a REFRESHING source — invariant 11.
type tokenPayload struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`

	// Refresh context. A grant without it degrades to a static token (usable
	// until expiry, then interactive re-authorization).
	TokenURL     string   `json:"token_url,omitempty"`
	AuthStyle    int      `json:"auth_style,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// persistGrant writes the grant through the store — invariant 11. Failures are
// logged, not returned; the write is detached from ctx's cancellation.
func persistGrant(ctx context.Context, s *store.McpServerStore, configID string, ocfg *oauth2.Config, tok *oauth2.Token) {
	ctx = context.WithoutCancel(ctx)
	log := logging.Ctx(ctx)
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
		log.Error("encoding MCP OAuth grant failed", "error", err, "mcp", configID)
		return
	}
	if err := s.SaveOAuthToken(ctx, configID, string(b)); err != nil {
		log.Error("persisting MCP OAuth grant failed; connection works now but won't survive a restart", "error", err, "mcp", configID)
	}
	// The response's "scope" is what the server ACTUALLY granted; a missing
	// scope otherwise surfaces only as an opaque error at tool-call time.
	if granted, _ := tok.Extra("scope").(string); granted != "" {
		log.Info("mcp oauth grant persisted", "mcp", configID, "granted_scopes", granted)
	}
}

// persistingTokenSource wraps a refreshing oauth2.TokenSource and re-persists
// the grant whenever a refresh produced a new token — invariant 11.
type persistingTokenSource struct {
	// ctx is what a refresh persists under: oauth2.TokenSource takes no
	// context, so it is captured here, detached from the caller's cancellation.
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
	// Persist outside the lock; concurrent callers of the same token see
	// changed=false. (inner is a ReuseTokenSource, which serializes the refresh.)
	if changed {
		persistGrant(p.ctx, p.store, p.configID, p.cfg, tok)
	}
	return tok, nil
}

// restoredTokenSource rebuilds a token source from a persisted grant: refreshing
// (invariant 11), static for a refresh-less valid token, nil for the interactive flow.
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
	// Mirror the SDK's refresh context: detached from the caller's
	// cancellation, carrying the HTTP client oauth2 should use.
	rctx := context.WithValue(context.WithoutCancel(ctx), oauth2.HTTPClient, hc)
	return newPersistingSource(rctx, ocfg.TokenSource(rctx, tok), ocfg, configID, s, tok)
}
