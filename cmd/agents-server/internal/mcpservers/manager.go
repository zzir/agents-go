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
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/mcp"
)

// Manager holds the active MCP server connections, keyed by config ID.
type Manager struct {
	// rootCtx bounds the connections' own lifetime (an in-flight handshake
	// included), independent of the request that triggered the connect.
	rootCtx context.Context
	mu      sync.RWMutex
	servers map[string]*mcp.Server
	// connecting marks in-flight handshakes with their cancel; the handshake
	// runs OUTSIDE mu (network I/O), this map dedups concurrent Connects.
	connecting map[string]*connectState
	// connectGen is bumped on every Disconnect so a handshake completing AFTER
	// its config was reconciled away is discarded, not installed.
	connectGen map[string]uint64
}

// connectState is one in-flight handshake: its cancel (so Disconnect can abort
// it) and the connectGen it captured (so a superseded result is discarded).
type connectState struct {
	cancel context.CancelFunc
	gen    uint64
}

// NewManager returns an empty manager; rootCtx scopes every connection's lifetime.
func NewManager(rootCtx context.Context) *Manager {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	return &Manager{
		rootCtx:    rootCtx,
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

// beginConnect claims the right to handshake id: done=true when already
// connected, ErrConnectInProgress when claimed; else the caller MUST finishConnect.
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

// finishConnect releases the claim and installs srv — unless the generation
// advanced meanwhile (a Disconnect superseded it), then srv is closed instead.
func (m *Manager) finishConnect(id string, gen uint64, srv *mcp.Server, connErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Clear the slot only if still ours: a newer beginConnect could not have
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

// Connect starts an MCP server connection from a stored config; a no-op when
// already connected. ctx bounds only the handshake: the connection lives until
// Disconnect, CloseAll or the root context ends.
func (m *Manager) Connect(ctx context.Context, cfg *store.McpServerConfig) error {
	// Validated before the connect slot is claimed.
	var hc store.HTTPMcpConfig
	if cerr := store.DecodeConfig(cfg.Config, &hc); cerr != nil {
		return fmt.Errorf("mcp server %s: invalid config: %w", cfg.Name, cerr)
	}
	transport := m.httpTransport(&hc, nil)
	opts := buildMcpOptions(cfg.Name, hc.McpRetryConfig, hc.UseStructuredContent)
	// Redial makes the connection self-healing (spec §2.16), on the manager's
	// own context.
	opts.Redial = func(context.Context) (mcpsdk.Transport, error) {
		return m.httpTransport(&hc, nil), nil
	}

	done, hctx, gen, err := m.beginConnect(ctx, cfg.ID)
	if err != nil || done {
		return err // already connected (nil) or another connect is in flight
	}

	// Handshake OUTSIDE the lock, under hctx (which Disconnect can cancel).
	srv, err := mcp.NewWithTransport(hctx, cfg.Name, transport, opts)
	if err != nil {
		err = fmt.Errorf("connecting MCP server %s: %w", cfg.Name, err)
	}
	return m.finishConnect(cfg.ID, gen, srv, err)
}

// Reconcile makes the live connection match a server's config after a write:
// the current connection is dropped, and an enabled server reconnects in the
// background off the root context (an OAuth one through the coordinator's
// silent path, waiting for the user when no saved token connects).
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
			// Empty origin = non-interactive: the saved token or needs-authorization.
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
		// The only trace a failed reconnect leaves: the write already returned.
		if err := m.Connect(ctx, &cfg); err != nil {
			logging.Ctx(ctx).Warn("mcp reconnect after config change failed", "error", err, "mcp", cfg.Name)
		}
	}()
}

// IsOAuthConfig reports whether cfg is a server using OAuth, which connects
// through the OAuth coordinator, never plain Connect.
func IsOAuthConfig(cfg *store.McpServerConfig) bool {
	var hc store.HTTPMcpConfig
	if len(cfg.Config) > 0 {
		_ = json.Unmarshal(cfg.Config, &hc)
	}
	return hc.AuthMode == "oauth"
}

// httpTransport builds the streamable transport for an HTTP server config; the
// first connect and every re-dial go through it, so a healed connection cannot drift.
func (m *Manager) httpTransport(hc *store.HTTPMcpConfig, oauthHandler auth.OAuthHandler) *mcpsdk.StreamableClientTransport {
	t := &mcpsdk.StreamableClientTransport{Endpoint: hc.Endpoint, OAuthHandler: oauthHandler}
	t.HTTPClient = httpClientFor(hc.Headers)
	return t
}

// httpClientFor builds an HTTP MCP transport's client: static headers, no
// client timeout (each call's bound is its context's).
func httpClientFor(headers map[string]string) *http.Client {
	var rt http.RoundTripper = &errorBodyRoundTripper{base: http.DefaultTransport}
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

// errorBodyRoundTripper logs an MCP server's error-response body (the status
// line alone says nothing); 401 and a GET's 405 stay quiet. The body is stitched back.
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
// OAuth handler, blocking until the flow completes or ctx ends (the
// coordinator calls it in a goroutine).
func (m *Manager) ConnectHTTPWithOAuth(ctx context.Context, cfg *store.McpServerConfig, hc *store.HTTPMcpConfig, oauthHandler auth.OAuthHandler) error {
	done, hctx, gen, err := m.beginConnect(ctx, cfg.ID)
	if err != nil || done {
		return err
	}

	transport := m.httpTransport(hc, oauthHandler)
	opts := buildMcpOptions(cfg.Name, hc.McpRetryConfig, hc.UseStructuredContent)
	// The same handler on the re-dial: a healed connection re-authorizes through
	// the handler that holds the persisting token source (invariant 11).
	hcCopy := *hc
	opts.Redial = func(context.Context) (mcpsdk.Transport, error) {
		return m.httpTransport(&hcCopy, oauthHandler), nil
	}

	srv, cerr := mcp.NewWithTransport(hctx, cfg.Name, transport, opts)
	if cerr != nil {
		cerr = fmt.Errorf("connecting MCP server %s with OAuth: %w", cfg.Name, cerr)
	}
	return m.finishConnect(cfg.ID, gen, srv, cerr)
}

// ToolPrefix is what a server's tool names carry in front when exposed to an
// agent: the server's name and a double underscore.
func ToolPrefix(serverName string) string { return serverName + "__" }

// buildMcpOptions is the one place every connection's mcp.Options is assembled,
// so a new option cannot be missed on the OAuth path.
func buildMcpOptions(name string, retry store.McpRetryConfig, useStructuredContent bool) mcp.Options {
	opts := mcp.Options{
		ToolNamePrefix:       ToolPrefix(name),
		MaxRetryAttempts:     retry.MaxRetryAttempts,
		UseStructuredContent: useStructuredContent,
		// Tools are listed every turn; one fetch, then memory until list_changed.
		CacheToolsList: true,
	}
	if retry.RetryBackoffMs > 0 {
		opts.RetryBackoffBase = time.Duration(retry.RetryBackoffMs) * time.Millisecond
	}
	return opts
}

// Disconnect closes an MCP server connection and removes it from the manager,
// invalidating any in-flight handshake for the id: its generation is bumped
// (a late completion is discarded) and its context cancelled.
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
// right now. A live call: the caller bounds it with a context deadline.
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

// IsConnecting reports whether a handshake for the given ID is in flight. An
// interactive OAuth flow holds the slot for its whole popup wait: a
// user-facing state checks the coordinator's IsAuthorizing first.
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

// ConnectEnabled connects every enabled stored MCP server (OAuth ones silently
// through the coordinator); a failure is logged and skipped. Run it in a goroutine.
func ConnectEnabled(ctx context.Context, mgr *Manager, servers *store.McpServerStore, oauth *OAuthCoordinator) {
	log := logging.Ctx(ctx)
	configs, err := servers.List(ctx)
	if err != nil {
		log.Warn("listing mcp servers for auto-connect", "error", err)
		return
	}
	// Concurrently, each under its own handshake timeout; the connection's
	// lifetime is the root context's.
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
