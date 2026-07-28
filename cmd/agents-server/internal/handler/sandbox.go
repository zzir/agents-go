package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	terminals         *TerminalHandler
}

// NewSandboxHandler returns a handler backed by the given store and sandbox manager.
// allowLocal controls whether type "local" sandboxes may be created.
func NewSandboxHandler(s *store.SandboxStore, m *bridge.SandboxManager, allowLocal bool) *SandboxHandler {
	return &SandboxHandler{store: s, manager: m, allowLocalSandbox: allowLocal}
}

// WithTerminals wires the terminal registry so Update/Delete also tear down
// live web terminals on the affected sandbox.
func (h *SandboxHandler) WithTerminals(t *TerminalHandler) *SandboxHandler {
	h.terminals = t
	return h
}

// closeTerminals tears down live web terminals for a sandbox config, if the
// terminal feature is wired.
func (h *SandboxHandler) closeTerminals(id string) {
	if h.terminals != nil {
		h.terminals.CloseSandboxTerminals(id)
	}
}

// terminalCapable reports whether a sandbox config can host an interactive
// web terminal: ssh always, docker only in persistent mode (an ephemeral
// container has nothing to attach to between Execs), local never — a web
// terminal on the host process is a bigger grant than --allow-local-sandbox
// implies.
func terminalCapable(cfg *store.SandboxConfig) bool {
	switch cfg.Type {
	case "ssh":
		return true
	case "docker":
		var dc store.DockerConfig
		if len(cfg.Config) > 0 {
			if err := json.Unmarshal(cfg.Config, &dc); err != nil {
				return false
			}
		}
		return dc.Persistent
	default:
		return false
	}
}

// List responds with all sandbox configurations, secrets masked.
//
//	@Summary	List sandboxes
//	@Tags		sandboxes
//	@Produce	json
//	@Success	200	{array}		store.SandboxConfig
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes [get]
func (h *SandboxHandler) List(c *gin.Context) {
	configs, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range configs {
		configs[i] = sanitizeSandboxConfig(configs[i])
		configs[i].Terminal = terminalCapable(&configs[i])
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

// validateSandbox checks required fields and type-level permissions. It
// reports the failure to c and returns false when the request is rejected.
func (h *SandboxHandler) validateSandbox(c *gin.Context, req *createSandboxReq) bool {
	if req.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	switch req.Type {
	case "local":
		if !h.allowLocalSandbox {
			forbidden(c, "local sandbox is disabled; start the server with --allow-local-sandbox to enable it")
			return false
		}
	case "docker":
		var dc struct {
			Host string `json:"host"`
		}
		if len(req.Config) > 0 {
			// A malformed docker config must be rejected, not ignored: swallowing
			// the error would leave dc.Host empty and let a remote-host config
			// slip past the block below.
			if err := json.Unmarshal(req.Config, &dc); err != nil {
				badRequest(c, "config is not valid JSON: "+err.Error())
				return false
			}
		}
		if dc.Host != "" {
			badRequest(c, "remote Docker daemon is not supported; use a local daemon or the SSH sandbox for remote hosts")
			return false
		}
	case "ssh":
		var sc store.SSHConfig
		if len(req.Config) > 0 {
			if err := json.Unmarshal(req.Config, &sc); err != nil {
				badRequest(c, "config is not valid JSON: "+err.Error())
				return false
			}
		}
		if sc.Addr == "" {
			badRequest(c, "ssh sandbox requires config.addr")
			return false
		}
	case "":
		badRequest(c, "type is required")
		return false
	default:
		badRequest(c, "type must be local, docker, or ssh, got "+req.Type)
		return false
	}
	return true
}

// Create persists a new sandbox configuration from the request body.
//
//	@Summary		Create sandbox
//	@Description	type: local (requires --allow-local-sandbox), docker, or ssh. config is backend-specific; the SSH password is write-only (******** mask semantics). All backends accept an optional max_read_file_bytes cap for the read_file tool (0 = 8 MiB default).
//	@Tags			sandboxes
//	@Accept			json
//	@Produce		json
//	@Param			sandbox	body		createSandboxReq	true	"Sandbox configuration"
//	@Success		201		{object}	store.SandboxConfig
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse	"local sandbox disabled"
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandboxes [post]
func (h *SandboxHandler) Create(c *gin.Context) {
	var req createSandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validateSandbox(c, &req) {
		return
	}
	cfg := req.toConfig()
	// No stored config yet: mask sentinels resolve to empty.
	cfg.Config = restoreSandboxConfig(cfg.Type, cfg.Config, nil)
	if err := h.store.Create(c.Request.Context(), cfg); err != nil {
		internalError(c, err)
		return
	}
	created := sanitizeSandboxConfig(*cfg)
	created.Terminal = terminalCapable(&created)
	c.JSON(http.StatusCreated, created)
}

// Get responds with the sandbox configuration identified by the id path
// parameter, secrets masked.
//
//	@Summary	Get sandbox
//	@Tags		sandboxes
//	@Produce	json
//	@Param		id	path		string	true	"Sandbox ID"
//	@Success	200	{object}	store.SandboxConfig
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id} [get]
func (h *SandboxHandler) Get(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	out := sanitizeSandboxConfig(*cfg)
	out.Terminal = terminalCapable(&out)
	c.JSON(http.StatusOK, out)
}

// Update overwrites the sandbox configuration identified by the id path
// parameter and responds with the updated configuration. A masked SSH
// password keeps the stored value.
//
//	@Summary	Update sandbox
//	@Tags		sandboxes
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string				true	"Sandbox ID"
//	@Param		sandbox	body		createSandboxReq	true	"Sandbox configuration"
//	@Success	200		{object}	store.SandboxConfig
//	@Failure	400		{object}	ErrorResponse
//	@Failure	403		{object}	ErrorResponse	"local sandbox disabled"
//	@Failure	404		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id} [put]
func (h *SandboxHandler) Update(c *gin.Context) {
	var req createSandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validateSandbox(c, &req) {
		return
	}
	id := c.Param("id")
	ctx := c.Request.Context()
	// Load the current row so masked secrets can round-trip to their stored
	// values. A transient (non-not-found) Get failure must abort: continuing
	// with an empty prev would resolve the ******** mask to "" and silently
	// WIPE the stored ssh password — the same guard every sibling handler
	// carries. Not-found falls through; the Update below returns 404 for it.
	var prevConfig json.RawMessage
	prev, err := h.store.Get(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(c, err)
		return
	}
	if prev != nil {
		prevConfig = prev.Config
	}
	cfg := req.toConfig()
	cfg.Config = restoreSandboxConfig(cfg.Type, cfg.Config, prevConfig)
	// Persist first, then drop the cached instance: if the DB write fails, the
	// live sandbox must stay as it was rather than be torn down for a change
	// that never landed. Remove() forces a rebuild with the new config on next
	// use.
	if err := h.store.Update(ctx, id, cfg); err != nil {
		storeError(c, err)
		return
	}
	h.manager.Remove(id)
	h.closeTerminals(id)
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	out := sanitizeSandboxConfig(*updated)
	out.Terminal = terminalCapable(&out)
	c.JSON(http.StatusOK, out)
}

// Delete removes the sandbox configuration identified by the id path parameter.
//
//	@Summary	Delete sandbox
//	@Tags		sandboxes
//	@Param		id	path	string	true	"Sandbox ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id} [delete]
func (h *SandboxHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	// Delete from the DB first: only tear down the live instance once the row
	// is gone, so a failed delete doesn't leave a persisted sandbox with its
	// running instance already closed.
	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		storeError(c, err)
		return
	}
	h.manager.Remove(id)
	h.closeTerminals(id)
	c.Status(http.StatusNoContent)
}

// Test runs a fixed health-check command in the sandbox to verify connectivity.
//
//	@Summary		Test sandbox
//	@Description	Runs "echo ok" in the sandbox. 200 with ok=false means the sandbox was reachable but the command failed.
//	@Tags			sandboxes
//	@Produce		json
//	@Param			id	path		string	true	"Sandbox ID"
//	@Success		200	{object}	sandboxTestResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse	"sandbox unreachable"
//	@Security		BearerAuth
//	@Router			/sandboxes/{id}/test [post]
func (h *SandboxHandler) Test(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}

	sb, err := h.manager.GetOrCreate(cfg)
	if err != nil {
		upstreamError(c, err)
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
		upstreamError(c, err)
		return
	}
	if res.ExitCode != 0 || res.TimedOut {
		detail := res.Stderr
		if res.TimedOut {
			detail = "timed out"
		}
		c.JSON(http.StatusOK, sandboxTestResp{OK: false, Detail: detail})
		return
	}
	c.JSON(http.StatusOK, sandboxTestResp{OK: true})
}

// sandboxTestResp is the Test response: whether the health-check command
// succeeded, with failure detail when it didn't.
type sandboxTestResp struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}
