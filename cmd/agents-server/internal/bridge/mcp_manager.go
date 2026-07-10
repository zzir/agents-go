package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/mcp"
)

// McpManager manages MCP server connections. It maintains a map of active
// connections keyed by config ID.
type McpManager struct {
	// rootCtx bounds the lifetime of the connections themselves (most
	// importantly the stdio subprocesses), independent of whichever request
	// context happened to trigger the connect.
	rootCtx  context.Context
	settings *store.SettingStore
	mu       sync.RWMutex
	servers  map[string]*mcp.Server
	// connecting marks servers whose handshake is in flight. The handshake
	// runs OUTSIDE mu (it does network/subprocess I/O and must not block
	// Get/IsConnected/Disconnect); this set dedups concurrent Connect calls
	// for the same server without holding the lock across the handshake.
	connecting map[string]bool
}

// NewMcpManager returns a new manager with no active connections. rootCtx
// scopes connection lifetimes: cancelling it stops every stdio subprocess.
func NewMcpManager(rootCtx context.Context, settings *store.SettingStore) *McpManager {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	return &McpManager{
		rootCtx:    rootCtx,
		settings:   settings,
		servers:    make(map[string]*mcp.Server),
		connecting: make(map[string]bool),
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
// owns the claim and MUST call finishConnect.
func (m *McpManager) beginConnect(id string) (done bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; ok {
		return true, nil
	}
	if m.connecting[id] {
		return false, ErrConnectInProgress
	}
	m.connecting[id] = true
	return false, nil
}

// finishConnect releases the claim and, on success, stores srv. A server that
// appeared meanwhile (should not happen given the claim) is closed rather than
// leaked.
func (m *McpManager) finishConnect(id string, srv *mcp.Server, connErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.connecting, id)
	if connErr != nil {
		return connErr
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
func (m *McpManager) Connect(ctx context.Context, cfg *store.McpServerConfig) error {
	// Validate config before claiming the connect slot so a bad config fails
	// fast and can't strand the in-progress flag.
	var cmd *exec.Cmd
	var transport *mcpsdk.StreamableClientTransport
	var retry store.McpRetryConfig
	var useStructured bool
	switch cfg.TransportType {
	case "stdio":
		var sc store.StdioMcpConfig
		if cerr := unmarshalConfig(cfg.Config, &sc); cerr != nil {
			return fmt.Errorf("mcp server %s: invalid config: %w", cfg.Name, cerr)
		}
		retry, useStructured = sc.McpRetryConfig, sc.UseStructuredContent
		// The command context governs the subprocess lifetime — it must be
		// the manager's root context, NOT the caller's (a request context
		// would kill the server as soon as the request ends).
		cmd = exec.CommandContext(m.rootCtx, sc.Command, sc.Args...)
	case "streamable_http":
		var hc store.HTTPMcpConfig
		if cerr := unmarshalConfig(cfg.Config, &hc); cerr != nil {
			return fmt.Errorf("mcp server %s: invalid config: %w", cfg.Name, cerr)
		}
		retry, useStructured = hc.McpRetryConfig, hc.UseStructuredContent
		transport = &mcpsdk.StreamableClientTransport{Endpoint: hc.Endpoint}
		transport.HTTPClient = httpClientFor(m.proxyClient(ctx), hc.Headers)
	default:
		return fmt.Errorf("unknown transport type: %s", cfg.TransportType)
	}
	opts := buildMcpOptions(cfg.Name, retry, useStructured)

	done, err := m.beginConnect(cfg.ID)
	if err != nil || done {
		return err // already connected (nil) or another connect is in flight
	}

	// Handshake OUTSIDE the lock: this does subprocess spawn / network I/O and
	// a slow server here must not block Get/IsConnected/Disconnect/Connect.
	var srv *mcp.Server
	switch cfg.TransportType {
	case "stdio":
		srv, err = mcp.NewStdioServer(ctx, cfg.Name, cmd, opts)
	case "streamable_http":
		srv, err = mcp.NewWithTransport(ctx, cfg.Name, transport, opts)
	}
	if err != nil {
		err = fmt.Errorf("connecting MCP server %s: %w", cfg.Name, err)
	}
	return m.finishConnect(cfg.ID, srv, err)
}

// Reconcile makes the live connection match a server's desired config after a
// config write: it always drops the current connection (so a changed endpoint /
// command / headers can't keep serving stale config) and, for an enabled
// server, reconnects in the background under a bounded deadline off the manager
// root context (the request context is already gone). A disabled server is left
// disconnected. OAuth servers reconnect through the coordinator's silent path —
// a saved token connects without a popup, and without one the server is left
// for the user to authorize interactively (only the frontend can drive that).
// Centralizing this here keeps handlers from imperatively sequencing
// Disconnect/Connect and getting the order wrong.
func (m *McpManager) Reconcile(desired *store.McpServerConfig, oauth *OAuthCoordinator) {
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
		if unmarshalConfig(cfg.Config, &hc) != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(m.rootCtx, mcpAutoConnectTimeout)
			defer cancel()
			// Empty origin = non-interactive: connect with the saved token or
			// report needs-authorization without parking a popup-wait goroutine.
			_, _ = oauth.ConnectWithOAuth(ctx, m, &cfg, &hc, "")
		}()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(m.rootCtx, mcpAutoConnectTimeout)
		defer cancel()
		_ = m.Connect(ctx, &cfg)
	}()
}

// IsOAuthConfig reports whether cfg is a streamable_http server using OAuth,
// which must be (re)connected through the OAuth coordinator rather than the
// plain Connect path.
func IsOAuthConfig(cfg *store.McpServerConfig) bool {
	if cfg.TransportType != "streamable_http" {
		return false
	}
	var hc store.HTTPMcpConfig
	if len(cfg.Config) > 0 {
		_ = json.Unmarshal(cfg.Config, &hc)
	}
	return hc.AuthMode == "oauth"
}

func (m *McpManager) proxyClient(ctx context.Context) *http.Client {
	return ProxyHTTPClient(ctx, m.settings)
}

// httpClientFor builds the HTTP client an HTTP-based MCP transport should use,
// combining the optional proxy client with optional static request headers.
// Returns nil (so the SDK uses its default client) when neither is configured.
func httpClientFor(proxy *http.Client, headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return proxy
	}
	base := http.DefaultTransport
	if proxy != nil && proxy.Transport != nil {
		base = proxy.Transport
	}
	client := &http.Client{Transport: &headerRoundTripper{base: base, headers: headers}}
	if proxy != nil {
		client.Timeout = proxy.Timeout
	}
	return client
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

// ConnectHTTPWithOAuth connects a streamable HTTP MCP server with the given
// OAuth handler. It is called from OAuthCoordinator in a goroutine and blocks
// until the OAuth flow completes (or the context is cancelled).
func (m *McpManager) ConnectHTTPWithOAuth(ctx context.Context, cfg *store.McpServerConfig, hc *store.HTTPMcpConfig, oauthHandler auth.OAuthHandler) error {
	done, err := m.beginConnect(cfg.ID)
	if err != nil || done {
		return err
	}

	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:     hc.Endpoint,
		OAuthHandler: oauthHandler,
	}
	transport.HTTPClient = httpClientFor(m.proxyClient(ctx), hc.Headers)

	srv, cerr := mcp.NewWithTransport(ctx, cfg.Name, transport, buildMcpOptions(cfg.Name, hc.McpRetryConfig, hc.UseStructuredContent))
	if cerr != nil {
		cerr = fmt.Errorf("connecting MCP server %s with OAuth: %w", cfg.Name, cerr)
	}
	return m.finishConnect(cfg.ID, srv, cerr)
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
	}
	if retry.RetryBackoffMs > 0 {
		opts.RetryBackoffBase = time.Duration(retry.RetryBackoffMs) * time.Millisecond
	}
	return opts
}

// Disconnect closes an MCP server connection and removes it from the manager.
func (m *McpManager) Disconnect(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	srv, ok := m.servers[id]
	if !ok {
		return nil
	}
	delete(m.servers, id)
	return srv.Close()
}

// Get returns a connected server by config ID, or nil if not connected.
func (m *McpManager) Get(id string) *mcp.Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.servers[id]
}

// IsConnected reports whether a server with the given ID is connected.
func (m *McpManager) IsConnected(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.servers[id]
	return ok
}

// IsConnecting reports whether a connection handshake for the given ID is in
// flight. Note an interactive OAuth flow holds the connect slot for its whole
// popup wait, so this stays true throughout — check the OAuth coordinator's
// IsAuthorizing first when deriving a user-facing state.
func (m *McpManager) IsConnecting(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connecting[id]
}

// CloseAll closes all active connections.
func (m *McpManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, srv := range m.servers {
		_ = srv.Close()
		delete(m.servers, id)
	}
}

// ConnectEnabledMcpServers connects every stored MCP server whose Enabled flag
// is true. Disabled servers are skipped. For OAuth servers with a saved token
// it uses the coordinator to reconnect silently. Failures are logged and
// skipped so one bad server cannot block the others (or server startup).
// Intended to be run in a goroutine.
func ConnectEnabledMcpServers(ctx context.Context, mgr *McpManager, servers *store.McpServerStore, oauth *OAuthCoordinator) {
	log := zerolog.Ctx(ctx)
	configs, err := servers.List(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("listing mcp servers for auto-connect")
		return
	}
	// Connect concurrently, each under its own handshake timeout: a hung or
	// unreachable server must not stall the others' auto-connect (it fails its
	// own deadline and the rest come up regardless). The timeout bounds only
	// the handshake — a stdio subprocess's lifetime is the manager root context.
	var wg sync.WaitGroup
	for i := range configs {
		cfg := &configs[i]
		if !cfg.Enabled {
			continue
		}
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, mcpAutoConnectTimeout)
			defer cancel()
			if cfg.TransportType == "streamable_http" && cfg.OAuthToken != "" {
				var hc store.HTTPMcpConfig
				if unmarshalConfig(cfg.Config, &hc) == nil && hc.AuthMode == "oauth" {
					result, err := oauth.ConnectWithOAuth(cctx, mgr, cfg, &hc, "")
					switch {
					case err != nil:
						log.Warn().Err(err).Str("mcp", cfg.Name).Msg("mcp oauth auto-connect failed")
					case result.Connected:
						log.Info().Str("mcp", cfg.Name).Msg("mcp oauth auto-connected with saved token")
					default:
						log.Warn().Str("mcp", cfg.Name).Msg("mcp oauth auto-connect needs user authorization, skipping")
					}
					return
				}
			}
			if err := mgr.Connect(cctx, cfg); err != nil {
				log.Warn().Err(err).Str("mcp", cfg.Name).Msg("mcp auto-connect failed")
			}
		})
	}
	wg.Wait()
}
