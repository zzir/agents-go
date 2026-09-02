// Package mcpservers keeps the live connections behind stored MCP server
// configs — connect, reconcile, heal — and the OAuth flow an HTTP server
// may demand before it talks.
package mcpservers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/mcp"
)

// Manager manages MCP server connections. It maintains a map of active
// connections keyed by config ID.
type Manager struct {
	// rootCtx bounds the lifetime of the connections themselves (most
	// importantly any in-flight handshake), independent of whichever request
	// context happened to trigger the connect.
	rootCtx  context.Context
	settings *settings.Reader
	mu       sync.RWMutex
	servers  map[string]*mcp.Server
	// connecting marks servers whose handshake is in flight, carrying the
	// handshake's cancel so Disconnect can abort it. The handshake runs OUTSIDE
	// mu (it does network I/O and must not block
	// Get/IsConnected/Disconnect); this map dedups concurrent Connect calls for
	// the same server without holding the lock across the handshake.
	connecting map[string]*connectState
	// connectGen is bumped on every Disconnect so a handshake that completes
	// AFTER its config was reconciled away is discarded rather than installed:
	// beginConnect captures the generation and finishConnect stores its result
	// only if it still matches.
	connectGen map[string]uint64
}

// connectState is one in-flight handshake: its cancel (so Disconnect can abort
// it) and the connectGen it captured (so a superseded result is discarded).
type connectState struct {
	cancel context.CancelFunc
	gen    uint64
}

// NewManager returns a new manager with no active connections. rootCtx
// scopes connection lifetimes: cancelling it severs every connection.
func NewManager(rootCtx context.Context, cfg *settings.Reader) *Manager {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	return &Manager{
		rootCtx:    rootCtx,
		settings:   cfg,
		servers:    make(map[string]*mcp.Server),
		connecting: make(map[string]*connectState),
		connectGen: make(map[string]uint64),
	}
}

// ErrConnectInProgress is returned by Connect/ConnectHTTPWithOAuth when another
// goroutine is already handshaking the same server.
var ErrConnectInProgress = fmt.Errorf("mcp connection already in progress")

// mcpAutoConnectTimeout bounds a single server's handshake during startup
// auto-connect, so one hung server can't delay the others.
const mcpAutoConnectTimeout = 30 * time.Second

// beginConnect claims the right to handshake id. It returns done=true if the
// server is already connected (caller should no-op), or ErrConnectInProgress if
// another goroutine holds the claim. On a nil error with done=false the caller
// owns the claim and MUST call finishConnect with the returned generation and a
// handshake bounded by the returned context (which Disconnect can cancel).
func (m *Manager) beginConnect(ctx context.Context, id string) (done bool, hctx context.Context, gen uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; ok {
		return true, nil, 0, nil
	}
	if m.connecting[id] != nil {
		return false, nil, 0, ErrConnectInProgress
	}
	hctx, cancel := context.WithCancel(ctx)
	gen = m.connectGen[id]
	m.connecting[id] = &connectState{cancel: cancel, gen: gen}
	return false, hctx, gen, nil
}

// finishConnect releases the claim and, on success, stores srv — UNLESS the
// server's generation advanced while we handshook (a Disconnect / reconcile
// superseded this attempt), in which case the fresh connection is closed and
// discarded so a reconfigured or disabled server is never left connected with
// stale config. A server that appeared meanwhile (should not happen given
// the claim) is likewise closed rather than leaked.
func (m *Manager) finishConnect(id string, gen uint64, srv *mcp.Server, connErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Clear the slot only if it is still ours: a Disconnect that cancelled us
	// leaves the entry for us to remove, but a newer beginConnect could not have
	// replaced it (the slot was held), so an equal-generation check is enough.
	if cs := m.connecting[id]; cs != nil && cs.gen == gen {
		delete(m.connecting, id)
	}
	if connErr != nil {
		return connErr
	}
	if m.connectGen[id] != gen {
		_ = srv.Close()
		return nil
	}
	if _, ok := m.servers[id]; ok {
		_ = srv.Close()
		return nil
	}
	m.servers[id] = srv
	return nil
}

// Connect creates and starts an MCP server connection from a stored config.
// If the server is already connected, this is a no-op. ctx only bounds the
// connection handshake; the connection itself lives until Disconnect,
// CloseAll, or the manager's root context ends.
func (m *Manager) Connect(ctx context.Context, cfg *store.McpServerConfig) error {
	// Validate config before claiming the connect slot so a bad config fails
	// fast and can't strand the in-progress flag.
	var hc store.HTTPMcpConfig
	if cerr := store.DecodeConfig(cfg.Config, &hc); cerr != nil {
		return fmt.Errorf("mcp server %s: invalid config: %w", cfg.Name, cerr)
	}
	transport := m.httpTransport(ctx, &hc, nil)
	opts := buildMcpOptions(cfg.Name, hc.McpRetryConfig, hc.UseStructuredContent)
	// Redial makes the connection self-healing (decisions §5.21): rebuilt on the
	// manager's own context — a re-dial minutes later must not reuse a
	// request context that is long gone.
	opts.Redial = func(context.Context) (mcpsdk.Transport, error) {
		return m.httpTransport(m.rootCtx, &hc, nil), nil
	}

	done, hctx, gen, err := m.beginConnect(ctx, cfg.ID)
	if err != nil || done {
		return err // already connected (nil) or another connect is in flight
	}

	// Handshake OUTSIDE the lock, under hctx (which Disconnect can cancel):
	// this does network I/O and a slow server here must not block
	// Get/IsConnected/Disconnect/Connect.
	srv, err := mcp.NewWithTransport(hctx, cfg.Name, transport, opts)
	if err != nil {
		err = fmt.Errorf("connecting MCP server %s: %w", cfg.Name, err)
	}
	return m.finishConnect(cfg.ID, gen, srv, err)
}

// Reconcile makes the live connection match a server's desired config after a
// config write: it always drops the current connection (so a changed endpoint
// or headers can't keep serving stale config) and, for an enabled
// server, reconnects in the background under a bounded deadline off the manager
// root context (the request context is already gone). A disabled server is left
// disconnected. OAuth servers reconnect through the coordinator's silent path —
// a saved token connects without a popup, and without one the server is left
// for the user to authorize interactively (only the frontend can drive that).
// Centralizing this here keeps handlers from imperatively sequencing
// Disconnect/Connect and getting the order wrong.
func (m *Manager) Reconcile(desired *store.McpServerConfig, oauth *OAuthCoordinator) {
	if desired == nil {
		return
	}
	_ = m.Disconnect(desired.ID)
	if !desired.Enabled {
		return
	}
	cfg := *desired
	if IsOAuthConfig(&cfg) {
		if oauth == nil || cfg.OAuthToken == "" {
			return
		}
		var hc store.HTTPMcpConfig
		if store.DecodeConfig(cfg.Config, &hc) != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(m.rootCtx, mcpAutoConnectTimeout)
			defer cancel()
			// Empty origin = non-interactive: connect with the saved token or
			// report needs-authorization without parking a popup-wait goroutine.
			result, err := oauth.ConnectWithOAuth(ctx, m, &cfg, &hc, "")
			switch {
			case err != nil:
				logging.Ctx(ctx).Warn("mcp oauth reconnect after config change failed", "error", err, "mcp", cfg.Name)
			case !result.Connected:
				logging.Ctx(ctx).Warn("mcp oauth reconnect after config change needs user authorization", "mcp", cfg.Name)
			}
		}()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(m.rootCtx, mcpAutoConnectTimeout)
		defer cancel()
		// The write already returned to the client, so this log is the only
		// trace a failed reconnect leaves.
		if err := m.Connect(ctx, &cfg); err != nil {
			logging.Ctx(ctx).Warn("mcp reconnect after config change failed", "error", err, "mcp", cfg.Name)
		}
	}()
}

// IsOAuthConfig reports whether cfg is a server using OAuth, which must be
// (re)connected through the OAuth coordinator rather than the plain Connect
// path.
func IsOAuthConfig(cfg *store.McpServerConfig) bool {
	var hc store.HTTPMcpConfig
	if len(cfg.Config) > 0 {
		_ = json.Unmarshal(cfg.Config, &hc)
	}
	return hc.AuthMode == "oauth"
}

func (m *Manager) proxyClient(ctx context.Context) *http.Client {
	return m.settings.ProxyClient(ctx)
}

// httpTransport builds the streamable transport for an HTTP server config. The
// first connect and every re-dial go through it, so a healed connection cannot
// drift from the original's endpoint, headers, proxy or OAuth handler.
func (m *Manager) httpTransport(ctx context.Context, hc *store.HTTPMcpConfig, oauthHandler auth.OAuthHandler) *mcpsdk.StreamableClientTransport {
	t := &mcpsdk.StreamableClientTransport{Endpoint: hc.Endpoint, OAuthHandler: oauthHandler}
	t.HTTPClient = httpClientFor(m.proxyClient(ctx), hc.Headers)
	return t
}

// httpClientFor builds the HTTP client an HTTP-based MCP transport should use:
// the proxy client's transport when one is set, plus optional static request
// headers. No client timeout — a streamable session holds its connection
// open, so each call's bound is its context's. Every client logs
// error-response bodies (see errorBodyRoundTripper).
func httpClientFor(proxy *http.Client, headers map[string]string) *http.Client {
	base := http.DefaultTransport
	if proxy != nil && proxy.Transport != nil {
		base = proxy.Transport
	}
	var rt http.RoundTripper = &errorBodyRoundTripper{base: base}
	if len(headers) > 0 {
		rt = &headerRoundTripper{base: rt, headers: headers}
	}
	return &http.Client{Transport: rt}
}

// headerRoundTripper adds a fixed set of headers to every request before
// delegating to base.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context()) // never mutate the caller's request
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// errorBodyRoundTripper logs the body of an error response from an MCP server.
// The transport's own error is just the status line ("Forbidden"), while the
// body spells out the actual reason — a disabled API, an insufficient scope.
// Two spec-sanctioned answers stay quiet: 401 (the normal authorization dance)
// and 405 to a GET (a server that offers no standalone SSE stream — the client
// proceeds without it). The sniffed bytes are stitched back so the caller reads
// the body unchanged.
type errorBodyRoundTripper struct {
	base http.RoundTripper
}

func (rt *errorBodyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if err != nil || resp.StatusCode < 400 || resp.StatusCode == http.StatusUnauthorized ||
		(resp.StatusCode == http.StatusMethodNotAllowed && req.Method == http.MethodGet) {
		return resp, err
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	logging.Ctx(req.Context()).Warn("mcp server error response",
		"url", req.URL.String(), "status", resp.Status, "body", strings.TrimSpace(string(head)))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	return resp, nil
}

// ConnectHTTPWithOAuth connects a streamable HTTP MCP server with the given
// OAuth handler. It is called from OAuthCoordinator in a goroutine and blocks
// until the OAuth flow completes (or the context is cancelled).
func (m *Manager) ConnectHTTPWithOAuth(ctx context.Context, cfg *store.McpServerConfig, hc *store.HTTPMcpConfig, oauthHandler auth.OAuthHandler) error {
	done, hctx, gen, err := m.beginConnect(ctx, cfg.ID)
	if err != nil || done {
		return err
	}

	transport := m.httpTransport(ctx, hc, oauthHandler)
	opts := buildMcpOptions(cfg.Name, hc.McpRetryConfig, hc.UseStructuredContent)
	// The same handler on the re-dial: a healed connection re-authorizes
	// through the machinery that authorized the first one — and that handler
	// holds the persisting token source, so a refresh still reaches the store.
	hcCopy := *hc
	opts.Redial = func(context.Context) (mcpsdk.Transport, error) {
		return m.httpTransport(m.rootCtx, &hcCopy, oauthHandler), nil
	}

	srv, cerr := mcp.NewWithTransport(hctx, cfg.Name, transport, opts)
	if cerr != nil {
		cerr = fmt.Errorf("connecting MCP server %s with OAuth: %w", cfg.Name, cerr)
	}
	return m.finishConnect(cfg.ID, gen, srv, cerr)
}

// buildMcpOptions is the single place every MCP connection's mcp.Options is
// assembled, so a new option can't be silently missed on the OAuth path. It sets
// the per-server tool-name prefix, retry policy and structured-content mode from
// the stored config.
func buildMcpOptions(name string, retry store.McpRetryConfig, useStructuredContent bool) mcp.Options {
	opts := mcp.Options{
		ToolNamePrefix:       name + "__",
		MaxRetryAttempts:     retry.MaxRetryAttempts,
		UseStructuredContent: useStructuredContent,
		// One fetch, then serve from memory: every chat turn lists each server's
		// tools, and the Context panel lists them on open — without the cache
		// each is a live round trip (a remote server put the panel >100ms). The
		// SDK invalidates on notifications/tools/list_changed, so a server that
		// changes its tools is still picked up.
		CacheToolsList: true,
	}
	if retry.RetryBackoffMs > 0 {
		opts.RetryBackoffBase = time.Duration(retry.RetryBackoffMs) * time.Millisecond
	}
	return opts
}

// Disconnect closes an MCP server connection and removes it from the manager.
// It also invalidates any in-flight handshake for the same id: the generation
// is bumped (so a handshake completing after this returns is discarded, not
// installed) and the handshake's context is cancelled (so it releases the
// connect slot promptly instead of after its own timeout).
func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	m.connectGen[id]++
	if cs := m.connecting[id]; cs != nil {
		cs.cancel()
	}
	srv, ok := m.servers[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.servers, id)
	m.mu.Unlock()
	return srv.Close()
}

// Get returns a connected server by config ID, or nil if not connected.
func (m *Manager) Get(id string) *mcp.Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.servers[id]
}

// ListToolsFor returns the server's display name and the tools it exposes
// right now, for a caller sizing its share of an agent's tool surface. It is a
// live call — MCP tools are the server's, not the agent's — so the caller bounds
// it with a context deadline.
func (m *Manager) ListToolsFor(ctx context.Context, id string) (string, []*agents.Tool, error) {
	srv := m.Get(id)
	if srv == nil {
		return "", nil, fmt.Errorf("mcp server %s is not connected", id)
	}
	tools, err := srv.ListTools(ctx, nil, nil)
	return srv.Name(), tools, err
}

// IsConnected reports whether a server with the given ID is connected.
func (m *Manager) IsConnected(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.servers[id]
	return ok
}

// IsConnecting reports whether a connection handshake for the given ID is in
// flight. Note an interactive OAuth flow holds the connect slot for its whole
// popup wait, so this stays true throughout — check the OAuth coordinator's
// IsAuthorizing first when deriving a user-facing state.
func (m *Manager) IsConnecting(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connecting[id] != nil
}

// CloseAll closes all active connections.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, srv := range m.servers {
		_ = srv.Close()
		delete(m.servers, id)
	}
}

// ConnectEnabled connects every stored MCP server whose Enabled flag
// is true. Disabled servers are skipped. For OAuth servers with a saved token
// it uses the coordinator to reconnect silently. Failures are logged and
// skipped so one bad server cannot block the others (or server startup).
// Intended to be run in a goroutine.
func ConnectEnabled(ctx context.Context, mgr *Manager, servers *store.McpServerStore, oauth *OAuthCoordinator) {
	log := logging.Ctx(ctx)
	configs, err := servers.List(ctx)
	if err != nil {
		log.Warn("listing mcp servers for auto-connect", "error", err)
		return
	}
	// Connect concurrently, each under its own handshake timeout: a hung or
	// unreachable server must not stall the others' auto-connect (it fails its
	// own deadline and the rest come up regardless). The timeout bounds only
	// the handshake — the connection's lifetime is the manager root context.
	var wg sync.WaitGroup
	for i := range configs {
		cfg := &configs[i]
		if !cfg.Enabled {
			continue
		}
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, mcpAutoConnectTimeout)
			defer cancel()
			if cfg.OAuthToken != "" {
				var hc store.HTTPMcpConfig
				if store.DecodeConfig(cfg.Config, &hc) == nil && hc.AuthMode == "oauth" {
					result, err := oauth.ConnectWithOAuth(cctx, mgr, cfg, &hc, "")
					switch {
					case err != nil:
						log.Warn("mcp oauth auto-connect failed", "error", err, "mcp", cfg.Name)
					case result.Connected:
						log.Info("mcp oauth auto-connected with saved token", "mcp", cfg.Name)
					default:
						log.Warn("mcp oauth auto-connect needs user authorization, skipping", "mcp", cfg.Name)
					}
					return
				}
			}
			if err := mgr.Connect(cctx, cfg); err != nil {
				log.Warn("mcp auto-connect failed", "error", err, "mcp", cfg.Name)
			}
		})
	}
	wg.Wait()
}
