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
	store   *store.SandboxStore
	manager *bridge.SandboxManager
}

// NewSandboxHandler returns a handler backed by the given store and sandbox manager.
func NewSandboxHandler(s *store.SandboxStore, m *bridge.SandboxManager) *SandboxHandler {
	return &SandboxHandler{store: s, manager: m}
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
	Name     string `json:"name"`
	Type     string `json:"type"`
	Host     string `json:"host"`
	Image    string `json:"image"`
	Network  bool   `json:"network"`
	RunCmd   string `json:"run_cmd"`
	Filename string `json:"filename"`
	Timeout  int    `json:"timeout"`
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
	cfg := &store.SandboxConfig{
		Name:     req.Name,
		Type:     req.Type,
		Host:     req.Host,
		Image:    req.Image,
		Network:  req.Network,
		RunCmd:   req.RunCmd,
		Filename: req.Filename,
		Timeout:  req.Timeout,
	}
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
	id := c.Param("id")
	h.manager.Remove(id)
	cfg := &store.SandboxConfig{
		Name:     req.Name,
		Type:     req.Type,
		Host:     req.Host,
		Image:    req.Image,
		Network:  req.Network,
		RunCmd:   req.RunCmd,
		Filename: req.Filename,
		Timeout:  req.Timeout,
	}
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

type execSandboxReq struct {
	Code string `json:"code"`
}

type execSandboxResp struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

// Exec runs the submitted code in the sandbox identified by the id path parameter.
func (h *SandboxHandler) Exec(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var req execSandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code is required"})
		return
	}

	sb, err := h.manager.GetOrCreate(cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filename := cfg.Filename
	if filename == "" {
		filename = "main.py"
	}
	var runCmd []string
	if cfg.RunCmd != "" {
		var parsed []string
		if jsonErr := json.Unmarshal([]byte(cfg.RunCmd), &parsed); jsonErr == nil && len(parsed) > 0 {
			runCmd = parsed
		}
	}
	if len(runCmd) == 0 {
		runCmd = []string{"python3", filename}
	}

	timeout := sandbox.DefaultTimeout
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout+5*time.Second)
	defer cancel()

	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Cmd:     runCmd,
		Files:   map[string]string{filename: req.Code},
		Timeout: timeout,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, execSandboxResp{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		TimedOut: res.TimedOut,
	})
}
