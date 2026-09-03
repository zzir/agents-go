package mcpservers

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

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

const oauthPendingTimeout = 5 * time.Minute

// silentRedirectURL satisfies the handler constructor on non-interactive
// (saved-token) connects; a registration endpoint may see it — never a live flow.
const silentRedirectURL = "http://127.0.0.1/mcp-oauth-silent-reconnect"

// oauthHTTPTimeout bounds each OAuth HTTP request. Refreshes run on a
// background context, so this client timeout is the only bound on a hung endpoint.
const oauthHTTPTimeout = 30 * time.Second

// oauthHTTPClient is the OAuth client: the proxy client's transport when one
// is set, bounded by oauthHTTPTimeout.
func oauthHTTPClient(proxy *http.Client) *http.Client {
	client := &http.Client{Timeout: oauthHTTPTimeout}
	if proxy != nil {
		client.Transport = proxy.Transport
	}
	return client
}

// Connect-attempt phases for the OAuth fetcher (see newConnectFetcher).
const (
	oauthPhaseSilent      int32 = iota // trying the saved grant; a popup can't be serviced
	oauthPhaseInteractive              // full flow in progress; park for the popup callback
	oauthPhaseEstablished              // connect resolved; re-auth needs a fresh user-driven flow
)

// newConnectFetcher builds one connect attempt's AuthorizationCodeFetcher and the
// phase it advances; only the interactive phase services a popup, and only once.
func newConnectFetcher(serverName string, urlCh chan string, codeCh chan *auth.AuthorizationResult) (auth.AuthorizationCodeFetcher, *atomic.Int32) {
	phase := &atomic.Int32{}
	var parked atomic.Bool
	fetcher := func(fctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		switch phase.Load() {
		case oauthPhaseInteractive:
			if !parked.CompareAndSwap(false, true) {
				return nil, fmt.Errorf("mcp server %q: authorization completed but was not accepted; verify the authorization server metadata (issuer / RFC 9207 iss support) and the server's required scopes or resource (audience)", serverName)
			}
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

	mu sync.Mutex
	// pending delivers the authorization code to the parked connect attempt,
	// keyed by OAuth state param.
	pending map[string]chan *auth.AuthorizationResult
	// inflight is the interactive attempt holding the connect slot, by server
	// config id, so a new authorize can supersede a stale one.
	inflight map[string]*oauthAttempt
}

// NewOAuthCoordinator creates a coordinator backed by the given store for
// token persistence.
func NewOAuthCoordinator(s *store.McpServerStore) *OAuthCoordinator {
	return &OAuthCoordinator{
		store:    s,
		pending:  make(map[string]chan *auth.AuthorizationResult),
		inflight: make(map[string]*oauthAttempt),
	}
}

// supersedeInflight cancels the interactive attempt running for id and waits
// for it to release the connect slot; else a stale one holds it 5 minutes.
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

// ConnectResult is returned by ConnectWithOAuth.
type ConnectResult struct {
	// Connected is true when the server connected without needing user
	// authorization (e.g. cached token, or no OAuth required).
	Connected bool
	// AuthorizeURL is non-empty when user authorization is needed; the caller
	// should open this URL in a popup.
	AuthorizeURL string
}

// ConnectWithOAuth attempts to connect an MCP server that uses OAuth: it
// creates the auth handler, kicks off the connection in a goroutine, and
// returns at once with either a Connected result or an AuthorizeURL for the
// frontend to open. redirectURI is the handler's absolute callback URL; empty
// means a non-interactive caller. ctx bounds the SILENT paths only (so a
// startup auto-connect's per-server timeout applies); the interactive flow
// outlives the request (see connectCtx below).
func (c *OAuthCoordinator) ConnectWithOAuth(
	ctx context.Context,
	mgr *Manager,
	cfg *store.McpServerConfig,
	hc *store.HTTPMcpConfig,
	redirectURI string,
) (*ConnectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// A non-interactive caller (startup auto-connect) cannot drive a popup; only
	// an interactive request supersedes a prior attempt and may park.
	interactive := redirectURI != ""
	if interactive {
		c.supersedeInflight(cfg.ID)
	} else {
		redirectURI = silentRedirectURL // see the const
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

	// The fetcher uses the SDK's own context (from connectCtx) — NOT the outer
	// request ctx, cancelled as soon as the handler returns the authorize URL.
	fetcher, phase := newConnectFetcher(cfg.Name, urlCh, codeCh)

	httpClient := oauthHTTPClient(mgr.proxyClient(context.Background()))

	handlerCfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirectURI,
		AuthorizationCodeFetcher: fetcher,
		Client:                   httpClient,
		// SEP-2207: request offline_access when the server advertises it, so
		// the grant carries a refresh token at all.
		RequestRefreshToken: true,
		// Persist the fresh grant (token plus the resolved client credentials and
		// token endpoint) and re-persist on every refresh — invariant 11.
		NewTokenSource: func(rctx context.Context, ocfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			// Persistence rides the caller's context for its logger, not rctx,
			// which the SDK derives from its own background root.
			persistGrant(ctx, c.store, cfg.ID, ocfg, tok)
			return newPersistingSource(ctx, ocfg.TokenSource(rctx, tok), ocfg, cfg.ID, c.store, tok), nil
		},
		// A restored grant refreshes through the same persisting machinery as a
		// live one (invariant 11); nil triggers the interactive flow.
		InitialTokenSource: restoredTokenSource(ctx, cfg.ID, cfg.OAuthToken, c.store, httpClient),
	}
	if preregistered != nil {
		handlerCfg.PreregisteredClient = preregistered
	} else {
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

	// A usable saved grant (refresh included) connects silently, honoring the
	// caller's ctx; a refresh inside Token() is bounded by httpClient's timeout.
	if ts, _ := authHandler.TokenSource(ctx); ts != nil {
		if tok, tokErr := ts.Token(); tokErr == nil && tok.Valid() {
			if err := mgr.ConnectHTTPWithOAuth(ctx, cfg, hc, authHandler); err == nil {
				phase.Store(oauthPhaseEstablished)
				return &ConnectResult{Connected: true}, nil
			}
			// Token rejected (revoked?) — fall through to full OAuth flow.
		}
	}

	// A non-interactive caller can't complete the browser flow: report it
	// WITHOUT parking a 5-minute goroutine on the connect slot.
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

	// Captured now: the flow outlives the request context that carries it.
	log := logging.Ctx(ctx)
	errCh := make(chan error, 1)
	go func() {
		// ConnectHTTPWithOAuth frees the connect slot before returning; only then
		// clear the attempt and close done, so a superseder can re-claim it.
		err := mgr.ConnectHTTPWithOAuth(connectCtx, cfg, hc, authHandler)
		// Resolved either way: any further Authorize on this handler is a
		// post-connect re-auth and fails fast (see newConnectFetcher).
		phase.Store(oauthPhaseEstablished)
		connectCancel()
		c.clearInflight(cfg.ID, attempt)
		close(attempt.done)
		// The one log line that says how the interactive flow ended.
		if err != nil {
			log.Warn("mcp oauth interactive connect ended without connecting", "error", err, "mcp", cfg.Name)
		} else {
			log.Info("mcp oauth interactive connect established", "mcp", cfg.Name)
		}
		errCh <- err
	}()

	select {
	case authURL := <-urlCh:
		state := extractState(authURL)

		c.mu.Lock()
		c.pending[state] = codeCh
		c.mu.Unlock()

		// Logs the exact redirect_uri the AS must send the browser back to —
		// the first thing to compare when no callback ever arrives.
		log.Info("mcp oauth authorization URL issued; awaiting browser callback", "mcp", cfg.Name, "redirect_uri", redirectURI)

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

		return &ConnectResult{AuthorizeURL: authURL}, nil

	case err := <-errCh:
		connectCancel()
		if err != nil {
			return nil, err
		}
		return &ConnectResult{Connected: true}, nil
	}
}

// HandleCallback delivers the authorization code to the pending connect
// goroutine exactly once; an unknown or consumed state is an error. iss is the
// RFC 9207 issuer from the redirect (empty when absent); the SDK rejects the
// exchange when its presence disagrees with the server's advertisement. The
// pending entry is removed under the lock BEFORE delivery, so a duplicate
// callback finds nothing rather than racing, and the send is non-blocking
// (codeCh is buffered, the fetcher receives at most once).
func (c *OAuthCoordinator) HandleCallback(state, code, iss string) error {
	c.mu.Lock()
	ch, ok := c.pending[state]
	if ok {
		delete(c.pending, state)
	}
	c.mu.Unlock()

	if !ok {
		return fmt.Errorf("no pending authorization for this state — it may have expired (5 min), been superseded by a newer authorize, or already been used; start authorization again from the MCP settings page")
	}

	select {
	case ch <- &auth.AuthorizationResult{Code: code, State: state, Iss: iss}:
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
