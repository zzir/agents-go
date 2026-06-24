package bridge

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/mcp"
)

// McpManager manages MCP server connections. It maintains a map of active
// connections keyed by config ID.
type McpManager struct {
	settings *store.SettingStore
	mu       sync.RWMutex
	servers  map[string]*mcp.Server
}

// NewMcpManager returns a new manager with no active connections.
func NewMcpManager(settings *store.SettingStore) *McpManager {
	return &McpManager{
		settings: settings,
		servers:  make(map[string]*mcp.Server),
	}
}

// Connect creates and starts an MCP server connection from a stored config.
// If the server is already connected, this is a no-op.
func (m *McpManager) Connect(ctx context.Context, cfg *store.McpServerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.servers[cfg.ID]; ok {
		return nil // already connected
	}

	var srv *mcp.Server
	var err error
	opts := mcp.Options{ToolNamePrefix: cfg.Name + "__"}

	switch cfg.TransportType {
	case "stdio":
		var sc store.StdioMcpConfig
		if cerr := unmarshalConfig(cfg.Config, &sc); cerr != nil {
			return fmt.Errorf("mcp server %s: invalid config: %w", cfg.Name, cerr)
		}
		cmd := exec.CommandContext(ctx, sc.Command, sc.Args...)
		srv, err = mcp.NewStdioServer(ctx, cfg.Name, cmd, opts)
	case "streamable_http":
		var hc store.HTTPMcpConfig
		if cerr := unmarshalConfig(cfg.Config, &hc); cerr != nil {
			return fmt.Errorf("mcp server %s: invalid config: %w", cfg.Name, cerr)
		}
		transport := &mcpsdk.StreamableClientTransport{Endpoint: hc.Endpoint}
		transport.HTTPClient = httpClientFor(m.proxyClient(ctx), hc.Headers)
		srv, err = mcp.NewWithTransport(ctx, cfg.Name, transport, opts)
	default:
		return fmt.Errorf("unknown transport type: %s", cfg.TransportType)
	}
	if err != nil {
		return fmt.Errorf("connecting MCP server %s: %w", cfg.Name, err)
	}

	m.servers[cfg.ID] = srv
	return nil
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
	m.mu.Lock()
	if _, ok := m.servers[cfg.ID]; ok {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:     hc.Endpoint,
		OAuthHandler: oauthHandler,
	}
	transport.HTTPClient = httpClientFor(m.proxyClient(ctx), hc.Headers)

	srv, err := mcp.NewWithTransport(ctx, cfg.Name, transport, mcp.Options{ToolNamePrefix: cfg.Name + "__"})
	if err != nil {
		return fmt.Errorf("connecting MCP server %s with OAuth: %w", cfg.Name, err)
	}

	m.mu.Lock()
	m.servers[cfg.ID] = srv
	m.mu.Unlock()
	return nil
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

// CloseAll closes all active connections.
func (m *McpManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, srv := range m.servers {
		_ = srv.Close()
		delete(m.servers, id)
	}
}

// AutoConnectMcpServers connects every stored MCP server whose config has
// AutoConnect set. For OAuth servers with a saved token it uses the coordinator
// to reconnect silently. Failures are logged and skipped so one bad server
// cannot block the others (or server startup). Intended to be run in a goroutine.
func AutoConnectMcpServers(ctx context.Context, mgr *McpManager, servers *store.McpServerStore, oauth *OAuthCoordinator) {
	log := zerolog.Ctx(ctx)
	configs, err := servers.List(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("listing mcp servers for auto-connect")
		return
	}
	for i := range configs {
		if !configs[i].AutoConnect {
			continue
		}
		cfg := &configs[i]
		if cfg.TransportType == "streamable_http" && cfg.OAuthToken != "" {
			var hc store.HTTPMcpConfig
			if unmarshalConfig(cfg.Config, &hc) == nil && hc.AuthMode == "oauth" {
				result, err := oauth.ConnectWithOAuth(ctx, mgr, cfg, &hc, "")
				if err != nil {
					log.Warn().Err(err).Str("mcp", cfg.Name).Msg("mcp oauth auto-connect failed")
				} else if result.Connected {
					log.Info().Str("mcp", cfg.Name).Msg("mcp oauth auto-connected with saved token")
				} else {
					log.Warn().Str("mcp", cfg.Name).Msg("mcp oauth auto-connect needs user authorization, skipping")
				}
				continue
			}
		}
		if err := mgr.Connect(ctx, cfg); err != nil {
			log.Warn().Err(err).Str("mcp", configs[i].Name).Msg("mcp auto-connect failed")
		}
	}
}
