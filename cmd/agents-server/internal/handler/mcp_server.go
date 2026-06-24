package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// McpServerHandler serves CRUD and connection endpoints for MCP servers.
type McpServerHandler struct {
	store   *store.McpServerStore
	manager *bridge.McpManager
	oauth   *bridge.OAuthCoordinator
}

// NewMcpServerHandler returns a handler backed by the given store and connection manager.
func NewMcpServerHandler(s *store.McpServerStore, m *bridge.McpManager, oc *bridge.OAuthCoordinator) *McpServerHandler {
	return &McpServerHandler{store: s, manager: m, oauth: oc}
}

type mcpServerListItem struct {
	store.McpServerConfig
	Connected bool   `json:"connected"`
	AuthState string `json:"auth_state,omitempty"`
}

// List responds with all MCP server configurations and their connection state.
func (h *McpServerHandler) List(c *gin.Context) {
	configs, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	items := make([]mcpServerListItem, len(configs))
	for i, cfg := range configs {
		items[i] = mcpServerListItem{
			McpServerConfig: cfg,
			Connected:       h.manager.IsConnected(cfg.ID),
			AuthState:       h.authState(&cfg),
		}
	}
	c.JSON(http.StatusOK, items)
}

// mcpServerReq is the request body for both Create and Update.
type mcpServerReq struct {
	Name          string `json:"name"`
	TransportType string `json:"transport_type"`
	AutoConnect   bool   `json:"auto_connect"`
	// Config is the transport-specific settings object, interpreted per
	// TransportType (see store.StdioMcpConfig / store.HTTPMcpConfig).
	Config json.RawMessage `json:"config"`
}

func (r *mcpServerReq) toModel() *store.McpServerConfig {
	return &store.McpServerConfig{
		Name:          r.Name,
		TransportType: r.TransportType,
		AutoConnect:   r.AutoConnect,
		Config:        r.Config,
	}
}

// Create persists a new MCP server configuration from the request body.
func (h *McpServerHandler) Create(c *gin.Context) {
	var req mcpServerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.TransportType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "transport_type is required"})
		return
	}
	cfg := req.toModel()
	if err := h.store.Create(c.Request.Context(), cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, cfg)
}

// Get responds with the MCP server configuration identified by the id path parameter.
func (h *McpServerHandler) Get(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, mcpServerListItem{
		McpServerConfig: *cfg,
		Connected:       h.manager.IsConnected(cfg.ID),
	})
}

// Update overwrites the MCP server configuration identified by the id path parameter.
func (h *McpServerHandler) Update(c *gin.Context) {
	var req mcpServerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Update(c.Request.Context(), c.Param("id"), req.toModel()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete disconnects and removes the MCP server identified by the id path parameter.
func (h *McpServerHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	// Disconnect first if connected.
	if err := h.manager.Disconnect(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Connect opens a connection to the MCP server identified by the id path parameter.
// For OAuth-enabled servers, it may return an authorize_url instead of connecting
// directly; the frontend should open that URL in a popup and wait for the callback.
func (h *McpServerHandler) Connect(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	if cfg.TransportType == "streamable_http" {
		var hc store.HTTPMcpConfig
		if err := json.Unmarshal(cfg.Config, &hc); err == nil && hc.AuthMode == "oauth" {
			scheme := "http"
			if c.Request.TLS != nil {
				scheme = "https"
			}
			origin := scheme + "://" + c.Request.Host
			result, err := h.oauth.ConnectWithOAuth(c.Request.Context(), h.manager, cfg, &hc, origin)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			if result.Connected {
				c.JSON(http.StatusOK, gin.H{"status": "connected"})
			} else {
				c.JSON(http.StatusOK, gin.H{
					"status":        "authorization_required",
					"authorize_url": result.AuthorizeURL,
					"state":         result.State,
				})
			}
			return
		}
	}

	if err := h.manager.Connect(c.Request.Context(), cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "connected"})
}

// Disconnect closes the connection to the MCP server identified by the id path parameter.
func (h *McpServerHandler) Disconnect(c *gin.Context) {
	if err := h.manager.Disconnect(c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// OAuthCallback handles the OAuth redirect from the authorization server.
// It delivers the authorization code to the pending connection and renders a
// small HTML page that notifies the opener window via postMessage.
func (h *McpServerHandler) OAuthCallback(c *gin.Context) {
	state := c.Query("state")
	code := c.Query("code")
	if state == "" || code == "" {
		c.String(http.StatusBadRequest, "missing state or code parameter")
		return
	}
	if errMsg := c.Query("error"); errMsg != "" {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackHTML("error", errMsg))
		return
	}
	if err := h.oauth.HandleCallback(state, code); err != nil {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackHTML("error", err.Error()))
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, oauthCallbackHTML("success", ""))
}

func (h *McpServerHandler) authState(cfg *store.McpServerConfig) string {
	if cfg.TransportType != "streamable_http" {
		return ""
	}
	var hc store.HTTPMcpConfig
	if err := json.Unmarshal(cfg.Config, &hc); err != nil || hc.AuthMode != "oauth" {
		return ""
	}
	if h.manager.IsConnected(cfg.ID) {
		return "authorized"
	}
	return "unauthorized"
}

func oauthCallbackHTML(status, errMsg string) string {
	return `<!DOCTYPE html><html><body><p id="msg">` +
		func() string {
			if status == "success" {
				return "Authorization successful. You can close this window."
			}
			return "Authorization failed: " + errMsg
		}() +
		`</p><script>
if (window.opener) {
  window.opener.postMessage({type:'mcp-oauth-done',status:'` + status + `'}, location.origin);
}
setTimeout(function(){ window.close(); }, 1500);
</script></body></html>`
}

type mcpToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Tools responds with the tools exposed by the connected MCP server.
func (h *McpServerHandler) Tools(c *gin.Context) {
	srv := h.manager.Get(c.Param("id"))
	if srv == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server not connected"})
		return
	}
	tools, err := srv.ListTools(c.Request.Context(), nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	items := make([]mcpToolInfo, len(tools))
	for i, t := range tools {
		info := mcpToolInfo{Name: t.ToolName()}
		if ft, ok := t.(*agents.FunctionTool); ok {
			info.Description = ft.Description
		}
		items[i] = info
	}
	c.JSON(http.StatusOK, items)
}
