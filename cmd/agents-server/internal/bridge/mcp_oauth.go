package bridge

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

const oauthPendingTimeout = 5 * time.Minute

// OAuthPending represents a pending OAuth authorization awaiting the callback.
type OAuthPending struct {
	AuthorizeURL string
	codeCh       chan *auth.AuthorizationResult
}

// OAuthCoordinator manages the asynchronous OAuth authorization code flow for
// MCP server connections. When a connection requires OAuth, it parks the
// authorization and returns the authorize URL; a later callback delivers the
// code and unblocks the connection.
type OAuthCoordinator struct {
	store *store.McpServerStore

	mu      sync.Mutex
	pending map[string]*OAuthPending // keyed by OAuth state param
}

// NewOAuthCoordinator creates a coordinator backed by the given store for
// token persistence.
func NewOAuthCoordinator(s *store.McpServerStore) *OAuthCoordinator {
	return &OAuthCoordinator{
		store:   s,
		pending: make(map[string]*OAuthPending),
	}
}

const oauthCallbackPath = "/api/mcp-servers/oauth/callback"

// RedirectURI builds the OAuth callback URL from the origin of the current
// HTTP request (scheme + host), so it works regardless of bind address.
func RedirectURI(requestOrigin string) string {
	return requestOrigin + oauthCallbackPath
}

// ConnectResult is returned by ConnectWithOAuth.
type ConnectResult struct {
	// Connected is true when the server connected without needing user
	// authorization (e.g. cached token, or no OAuth required).
	Connected bool
	// AuthorizeURL is non-empty when user authorization is needed; the caller
	// should open this URL in a popup.
	AuthorizeURL string
	// State is the OAuth state parameter, used to match the callback.
	State string
}

// ConnectWithOAuth attempts to connect an MCP server that uses OAuth. It
// creates the auth handler, kicks off the connection in a goroutine, and
// returns immediately with either a Connected result or an AuthorizeURL that
// the frontend should open.
// ctx bounds the SILENT paths — the saved-token direct connect below — so a
// caller's deadline (e.g. startup auto-connect's per-server timeout) actually
// applies. It must NOT bound the interactive full flow, which outlives the
// request that started it (see connectCtx below).
func (c *OAuthCoordinator) ConnectWithOAuth(
	ctx context.Context,
	mgr *McpManager,
	cfg *store.McpServerConfig,
	hc *store.HTTPMcpConfig,
	requestOrigin string,
) (*ConnectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var preregistered *oauthex.ClientCredentials
	if hc.OAuthClientID != "" {
		preregistered = &oauthex.ClientCredentials{
			ClientID: hc.OAuthClientID,
		}
		if hc.OAuthClientSecret != "" {
			preregistered.ClientSecretAuth = &oauthex.ClientSecretAuth{
				ClientSecret: hc.OAuthClientSecret,
			}
		}
	}

	codeCh := make(chan *auth.AuthorizationResult, 1)
	urlCh := make(chan string, 1)

	// The SDK passes its own context derived from connectCtx (5-min timeout).
	// We must use that — NOT the outer ctx which is the HTTP request context
	// and gets cancelled as soon as the handler returns the authorize URL.
	fetcher := func(fctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		urlCh <- args.URL

		select {
		case result := <-codeCh:
			return result, nil
		case <-fctx.Done():
			return nil, fctx.Err()
		}
	}

	handlerCfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              RedirectURI(requestOrigin),
		AuthorizationCodeFetcher: fetcher,
	}
	if preregistered != nil {
		handlerCfg.PreregisteredClient = preregistered
	} else {
		redirectURI := RedirectURI(requestOrigin)
		handlerCfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "agents-server",
				RedirectURIs: []string{redirectURI},
				Scope:        hc.OAuthScopes,
			},
		}
	}

	proxyClient := mgr.proxyClient(context.Background())
	if proxyClient != nil {
		handlerCfg.Client = proxyClient
	}

	authHandler, err := auth.NewAuthorizationCodeHandler(handlerCfg)
	if err != nil {
		return nil, fmt.Errorf("creating oauth handler: %w", err)
	}

	// Wrap with persistence: pre-populates from saved token and saves after
	// a successful authorize.
	handler := newPersistentOAuthHandler(authHandler, cfg.ID, c.store, cfg.OAuthToken)

	// If we have a valid saved token, try connecting directly — no popup needed.
	// This is a silent, synchronous path, so it honors the caller's ctx: startup
	// auto-connect passes a per-server timeout, so one hung server can't stall
	// the others.
	if ts, _ := handler.TokenSource(ctx); ts != nil {
		if tok, _ := ts.Token(); tok != nil && tok.Valid() {
			if err := mgr.ConnectHTTPWithOAuth(ctx, cfg, hc, handler); err == nil {
				return &ConnectResult{Connected: true}, nil
			}
			// Token rejected (expired / revoked) — fall through to full OAuth flow.
		}
	}

	connectCtx, connectCancel := context.WithTimeout(context.Background(), oauthPendingTimeout)

	errCh := make(chan error, 1)
	go func() {
		defer connectCancel()
		errCh <- mgr.ConnectHTTPWithOAuth(connectCtx, cfg, hc, handler)
	}()

	select {
	case authURL := <-urlCh:
		state := extractState(authURL)

		c.mu.Lock()
		c.pending[state] = &OAuthPending{
			AuthorizeURL: authURL,
			codeCh:       codeCh,
		}
		c.mu.Unlock()

		go func() {
			timer := time.NewTimer(oauthPendingTimeout)
			defer timer.Stop()
			select {
			case <-errCh:
			case <-timer.C:
			}
			c.mu.Lock()
			delete(c.pending, state)
			c.mu.Unlock()
		}()

		return &ConnectResult{
			AuthorizeURL: authURL,
			State:        state,
		}, nil

	case err := <-errCh:
		connectCancel()
		if err != nil {
			return nil, err
		}
		return &ConnectResult{Connected: true}, nil
	}
}

// HandleCallback processes the OAuth authorization callback. It delivers the
// code to the pending connection goroutine. Returns an error if the state is
// unknown or expired.
func (c *OAuthCoordinator) HandleCallback(state, code string) error {
	c.mu.Lock()
	p, ok := c.pending[state]
	c.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown or expired oauth state")
	}

	p.codeCh <- &auth.AuthorizationResult{
		Code:  code,
		State: state,
	}
	return nil
}

func extractState(authURL string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}
