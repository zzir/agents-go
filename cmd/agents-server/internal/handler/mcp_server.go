package handler

import (
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
}

// NewMcpServerHandler returns a handler backed by the given store and connection manager.
func NewMcpServerHandler(s *store.McpServerStore, m *bridge.McpManager) *McpServerHandler {
	return &McpServerHandler{store: s, manager: m}
}

type mcpServerListItem struct {
	store.McpServerConfig
	Connected bool `json:"connected"`
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
		}
	}
	c.JSON(http.StatusOK, items)
}

// mcpServerReq is the request body for both Create and Update.
type mcpServerReq struct {
	Name          string `json:"name"`
	TransportType string `json:"transport_type"`
	Command       string `json:"command"`
	Args          string `json:"args"`
	Endpoint      string `json:"endpoint"`
	OptionsJSON   string `json:"options"`
	AutoConnect   bool   `json:"auto_connect"`
}

func (r *mcpServerReq) toModel() *store.McpServerConfig {
	return &store.McpServerConfig{
		Name:          r.Name,
		TransportType: r.TransportType,
		Command:       r.Command,
		Args:          r.Args,
		Endpoint:      r.Endpoint,
		OptionsJSON:   r.OptionsJSON,
		AutoConnect:   r.AutoConnect,
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
func (h *McpServerHandler) Connect(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err := h.manager.Connect(c.Request.Context(), cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Disconnect closes the connection to the MCP server identified by the id path parameter.
func (h *McpServerHandler) Disconnect(c *gin.Context) {
	if err := h.manager.Disconnect(c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
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
