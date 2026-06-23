package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sync"

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
	opts := mcp.Options{}

	switch cfg.TransportType {
	case "stdio":
		args := parseArgs(cfg.Args)
		cmd := exec.CommandContext(ctx, cfg.Command, args...)
		srv, err = mcp.NewStdioServer(ctx, cfg.Name, cmd, opts)
	case "sse":
		transport := &mcpsdk.SSEClientTransport{Endpoint: cfg.Endpoint}
		if pc := m.proxyClient(ctx); pc != nil {
			transport.HTTPClient = pc
		}
		srv, err = mcp.NewWithTransport(ctx, cfg.Name, transport, opts)
	case "streamable_http":
		transport := &mcpsdk.StreamableClientTransport{Endpoint: cfg.Endpoint}
		if pc := m.proxyClient(ctx); pc != nil {
			transport.HTTPClient = pc
		}
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
// AutoConnect set. Failures are logged and skipped so one bad server cannot
// block the others (or server startup). Intended to be run in a goroutine.
func AutoConnectMcpServers(ctx context.Context, mgr *McpManager, servers *store.McpServerStore) {
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
		if err := mgr.Connect(ctx, &configs[i]); err != nil {
			log.Warn().Err(err).Str("mcp", configs[i].Name).Msg("mcp auto-connect failed")
		}
	}
}

func parseArgs(argsJSON string) []string {
	if argsJSON == "" {
		return nil
	}
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil
	}
	return args
}
