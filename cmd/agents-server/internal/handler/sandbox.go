package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// SandboxHandler serves CRUD endpoints and code execution for sandboxes.
type SandboxHandler struct {
	store             *store.SandboxStore
	manager           *bridge.SandboxManager
	allowLocalSandbox bool
}

// NewSandboxHandler returns a handler backed by the given store and sandbox manager.
// allowLocal controls whether type "local" sandboxes may be created.
func NewSandboxHandler(s *store.SandboxStore, m *bridge.SandboxManager, allowLocal bool) *SandboxHandler {
	return &SandboxHandler{store: s, manager: m, allowLocalSandbox: allowLocal}
}

// List responds with all sandbox configurations.
func (h *SandboxHandler) List(c *gin.Context) {
	configs, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, configs)
}

type createSandboxReq struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

func (r createSandboxReq) toConfig() *store.SandboxConfig {
	return &store.SandboxConfig{
		Name:   r.Name,
		Type:   r.Type,
		Config: r.Config,
	}
}

// validateSandboxReq checks type-level permissions. Returns an HTTP status and
// error message on failure, or (0, "") when the request is acceptable.
func (h *SandboxHandler) validateSandboxReq(req *createSandboxReq) (int, string) {
	if req.Type == "local" && !h.allowLocalSandbox {
		return http.StatusForbidden, "local sandbox is disabled; start the server with --allow-local-sandbox to enable it"
	}
	if req.Type == "docker" {
		var dc struct {
			Host string `json:"host"`
		}
		if len(req.Config) > 0 {
			_ = json.Unmarshal(req.Config, &dc)
		}
		if dc.Host != "" {
			return http.StatusBadRequest, "remote Docker daemon is not supported; use a local daemon or the SSH sandbox for remote hosts"
		}
	}
	return 0, ""
}

// Create persists a new sandbox configuration from the request body.
func (h *SandboxHandler) Create(c *gin.Context) {
	var req createSandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Type == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "type is required"})
		return
	}
	if code, msg := h.validateSandboxReq(&req); code != 0 {
		c.JSON(code, gin.H{"error": msg})
		return
	}
	cfg := req.toConfig()
	if err := h.store.Create(c.Request.Context(), cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, cfg)
}

// Get responds with the sandbox configuration identified by the id path parameter.
func (h *SandboxHandler) Get(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// Update overwrites the sandbox configuration identified by the id path parameter.
func (h *SandboxHandler) Update(c *gin.Context) {
	var req createSandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if code, msg := h.validateSandboxReq(&req); code != 0 {
		c.JSON(code, gin.H{"error": msg})
		return
	}
	id := c.Param("id")
	h.manager.Remove(id)
	cfg := req.toConfig()
	if err := h.store.Update(c.Request.Context(), id, cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete removes the sandbox configuration identified by the id path parameter.
func (h *SandboxHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	h.manager.Remove(id)
	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Test runs a fixed health-check command in the sandbox to verify connectivity.
func (h *SandboxHandler) Test(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	sb, err := h.manager.GetOrCreate(cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	timeout := sandbox.DefaultTimeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout+5*time.Second)
	defer cancel()

	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Cmd:     []string{"bash", "-c", "echo ok"},
		Timeout: timeout,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if res.ExitCode != 0 || res.TimedOut {
		detail := res.Stderr
		if res.TimedOut {
			detail = "timed out"
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "detail": detail})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
