package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

const oauthPendingTimeout = 5 * time.Minute

// oauthHTTPTimeout bounds each OAuth HTTP request (metadata discovery, client
// registration, code exchange, token refresh). Refreshes run on a background
// context — the oauth2 library retains the context passed at TokenSource
// construction — so this client timeout is the only bound on a hung token
// endpoint.
const oauthHTTPTimeout = 30 * time.Second

// oauthHTTPClient wraps the optional proxy client with the OAuth timeout.
func oauthHTTPClient(proxy *http.Client) *http.Client {
	client := &http.Client{Timeout: oauthHTTPTimeout}
	if proxy != nil {
		client.Transport = proxy.Transport
		if proxy.Timeout > 0 {
			client.Timeout = proxy.Timeout
		}
	}
	return client
}

// Connect-attempt phases for the OAuth fetcher (see newConnectFetcher).
const (
	oauthPhaseSilent      int32 = iota // trying the saved grant; a popup can't be serviced
	oauthPhaseInteractive              // full flow in progress; park for the popup callback
	oauthPhaseEstablished              // connect resolved; re-auth needs a fresh user-driven flow
)

// newConnectFetcher builds the AuthorizationCodeFetcher for one connect
// attempt, plus the phase the attempt advances as it progresses. Only the
// interactive phase can service an authorization popup — the fetcher then
// publishes the authorize URL on urlCh and parks until the OAuth callback
// delivers the code on codeCh. In every other phase Authorize means the saved
// grant was rejected (a 401 the refresh token could not fix), and nobody is
// watching urlCh: fail the request fast instead of parking it until its
// context expires. During the silent phase that failure makes the caller fall
// through to the interactive flow immediately; once established, the error
// tells the user to re-authorize from the MCP settings page, which starts a
// fresh flow.
func newConnectFetcher(serverName string, urlCh chan string, codeCh chan *auth.AuthorizationResult) (auth.AuthorizationCodeFetcher, *atomic.Int32) {
	phase := &atomic.Int32{}
	fetcher := func(fctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		switch phase.Load() {
		case oauthPhaseInteractive:
			urlCh <- args.URL
			select {
			case result := <-codeCh:
				return result, nil
			case <-fctx.Done():
				return nil, fctx.Err()
			}
		case oauthPhaseSilent:
			return nil, fmt.Errorf("mcp server %q: saved authorization no longer accepted", serverName)
		default:
			return nil, fmt.Errorf("mcp server %q: authorization no longer accepted and re-authorizing requires user action; re-authorize the server from the MCP settings page", serverName)
		}
	}
	return fetcher, phase
}

// OAuthPending represents a pending OAuth authorization awaiting the callback.
type OAuthPending struct {
	AuthorizeURL string
	codeCh       chan *auth.AuthorizationResult
}

// oauthAttempt tracks one interactive OAuth flow in progress for a server, so a
// repeat authorize can cancel and supersede it.
type oauthAttempt struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once the attempt released the connect slot
}

// OAuthCoordinator manages the asynchronous OAuth authorization code flow for
// MCP server connections. When a connection requires OAuth, it parks the
// authorization and returns the authorize URL; a later callback delivers the
// code and unblocks the connection.
type OAuthCoordinator struct {
	store *store.McpServerStore

	mu      sync.Mutex
	pending map[string]*OAuthPending // keyed by OAuth state param
	// inflight is the interactive attempt currently holding the connect slot,
	// keyed by server config id, so a new authorize for the same server can
	// supersede a stale one (e.g. the user refreshed the page mid-flow).
	inflight map[string]*oauthAttempt
}

// NewOAuthCoordinator creates a coordinator backed by the given store for
// token persistence.
func NewOAuthCoordinator(s *store.McpServerStore) *OAuthCoordinator {
	return &OAuthCoordinator{
		store:    s,
		pending:  make(map[string]*OAuthPending),
		inflight: make(map[string]*oauthAttempt),
	}
}

// supersedeInflight cancels the interactive OAuth attempt already running for id
// (if any) and waits for it to release the manager's connect slot, so a fresh
// authorize can claim it. Without this a stale attempt — the user refreshed the
// page or navigated away mid-flow — would hold the slot until its 5-minute
// timeout, and every retry would fail with ErrConnectInProgress.
func (c *OAuthCoordinator) supersedeInflight(id string) {
	c.mu.Lock()
	prev := c.inflight[id]
	delete(c.inflight, id)
	c.mu.Unlock()
	if prev == nil {
		return
	}
	prev.cancel()
	<-prev.done // the attempt's goroutine closes this after finishConnect frees the slot
}

// IsAuthorizing reports whether an interactive OAuth flow for the given server
// config id is in progress (an authorize popup is pending user action). A nil
// coordinator has no flows.
func (c *OAuthCoordinator) IsAuthorizing(id string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[id] != nil
}

// clearInflight removes a finished attempt, but only if it is still the current
// one (a superseding attempt may have already replaced or deleted it).
func (c *OAuthCoordinator) clearInflight(id string, a *oauthAttempt) {
	c.mu.Lock()
	if c.inflight[id] == a {
		delete(c.inflight, id)
	}
	c.mu.Unlock()
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
	// An empty origin means a non-interactive caller (startup auto-connect) that
	// cannot drive a browser popup; only an interactive request supersedes a
	// prior attempt and may park the popup-wait goroutine.
	interactive := requestOrigin != ""
	if interactive {
		c.supersedeInflight(cfg.ID)
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

	// The SDK passes the fetcher its own context derived from connectCtx
	// (5-min timeout). It must use that — NOT the outer ctx which is the HTTP
	// request context and gets cancelled as soon as the handler returns the
	// authorize URL.
	fetcher, phase := newConnectFetcher(cfg.Name, urlCh, codeCh)

	httpClient := oauthHTTPClient(mgr.proxyClient(context.Background()))

	handlerCfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              RedirectURI(requestOrigin),
		AuthorizationCodeFetcher: fetcher,
		Client:                   httpClient,
		// SEP-2207: request offline_access when the server advertises it, so
		// the grant carries a refresh token at all.
		RequestRefreshToken: true,
		// Persist the fresh grant — token plus the resolved client
		// credentials and token endpoint, which for a dynamically registered
		// client exist nowhere else — then keep re-persisting on every
		// refresh. rctx is the SDK's background refresh context.
		NewTokenSource: func(rctx context.Context, ocfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			persistGrant(c.store, cfg.ID, ocfg, tok)
			return newPersistingSource(ocfg.TokenSource(rctx, tok), ocfg, cfg.ID, c.store, tok), nil
		},
		// A restored grant refreshes through the same persisting machinery as
		// a live one, so the restart and live paths are a single mechanism.
		// nil (no usable saved grant) triggers the interactive flow.
		InitialTokenSource: restoredTokenSource(cfg.ID, cfg.OAuthToken, c.store, httpClient),
	}
	if preregistered != nil {
		handlerCfg.PreregisteredClient = preregistered
	} else {
		redirectURI := RedirectURI(requestOrigin)
		handlerCfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "agents-go",
				RedirectURIs: []string{redirectURI},
				Scope:        hc.OAuthScopes,
				// RFC 7591 defaults to authorization_code only; without
				// advertising refresh_token many servers never issue one.
				GrantTypes: []string{"authorization_code", "refresh_token"},
			},
		}
	}

	authHandler, err := auth.NewAuthorizationCodeHandler(handlerCfg)
	if err != nil {
		return nil, fmt.Errorf("creating oauth handler: %w", err)
	}

	// If the saved grant yields a usable token — including refreshing an
	// expired access token — connect directly, no popup. This is a silent,
	// synchronous path honoring the caller's ctx for the connect itself
	// (startup auto-connect passes a per-server timeout, so one hung server
	// can't stall the others); a refresh inside Token() is bounded by
	// httpClient's timeout instead, as oauth2.TokenSource carries no per-call
	// context.
	if ts, _ := authHandler.TokenSource(ctx); ts != nil {
		if tok, tokErr := ts.Token(); tokErr == nil && tok.Valid() {
			if err := mgr.ConnectHTTPWithOAuth(ctx, cfg, hc, authHandler); err == nil {
				phase.Store(oauthPhaseEstablished)
				return &ConnectResult{Connected: true}, nil
			}
			// Token rejected (revoked?) — fall through to full OAuth flow.
		}
	}

	// A non-interactive caller can't complete the browser flow, so report that
	// authorization is needed WITHOUT parking a 5-minute goroutine — one would
	// hold the connect slot until timeout and block the user's later authorize.
	if !interactive {
		return &ConnectResult{Connected: false}, nil
	}

	// From here the popup flow owns the fetcher: Authorize may park for the
	// callback. Must be stored before the goroutine's transport can 401.
	phase.Store(oauthPhaseInteractive)

	connectCtx, connectCancel := context.WithTimeout(context.Background(), oauthPendingTimeout)
	attempt := &oauthAttempt{cancel: connectCancel, done: make(chan struct{})}
	c.mu.Lock()
	c.inflight[cfg.ID] = attempt
	c.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		// ConnectHTTPWithOAuth releases the manager's connect slot (finishConnect)
		// before returning; only then clear the attempt and close done, so a
		// superseding authorize that waits on done can safely re-claim the slot.
		err := mgr.ConnectHTTPWithOAuth(connectCtx, cfg, hc, authHandler)
		// The connect resolved either way; from here any Authorize on this
		// handler is a post-connect re-auth and must fail fast (see
		// newConnectFetcher).
		phase.Store(oauthPhaseEstablished)
		connectCancel()
		c.clearInflight(cfg.ID, attempt)
		close(attempt.done)
		errCh <- err
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
// code to the pending connection goroutine exactly once. Returns an error if
// the state is unknown or already consumed. iss is the RFC 9207 issuer from
// the redirect's "iss" query parameter (empty when the server sent none); the
// SDK rejects the exchange when its presence disagrees with the server's
// advertised authorization_response_iss_parameter_supported.
//
// The pending entry is removed under the lock BEFORE delivery, so a duplicate
// callback (browsers re-issue redirects; a user may reload the callback tab)
// finds nothing and returns the unknown-state error instead of racing. The send
// itself is non-blocking: codeCh is buffered (cap 1) and the fetcher receives
// at most once, so a redundant delivery must never park this goroutine forever
// on a full channel.
func (c *OAuthCoordinator) HandleCallback(state, code, iss string) error {
	c.mu.Lock()
	p, ok := c.pending[state]
	if ok {
		delete(c.pending, state)
	}
	c.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown or expired oauth state")
	}

	select {
	case p.codeCh <- &auth.AuthorizationResult{Code: code, State: state, Iss: iss}:
	default:
		// The fetcher already received (or gave up) — nothing to deliver to.
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
