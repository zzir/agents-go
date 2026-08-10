package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
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
	// workspaceAbs is the server --workspace directory, absolute — the default
	// workdir reported for local and persistent-docker sandboxes.
	workspaceAbs string
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

// WithWorkspace records the server workspace directory so responses can report
// each sandbox's default workdir.
func (h *SandboxHandler) WithWorkspace(dir string) *SandboxHandler {
	if abs, err := filepath.Abs(dir); err == nil {
		h.workspaceAbs = abs
	} else {
		h.workspaceAbs = dir
	}
	return h
}

// closeSandboxTerminals tears down live web terminals opened under a config
// generation below minGen, if the terminal feature is wired — and moves the
// registry's fence so a terminal still dialing cannot register afterwards.
func (h *SandboxHandler) closeSandboxTerminals(id string, minGen int64) {
	if h.terminals != nil {
		h.terminals.CloseSandboxTerminals(id, minGen)
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

// annotate fills the computed, never-stored response fields: terminal
// capability, plus the default workdir a session binding would use and whether
// a custom per-session workdir is honored (local/ssh only). The workdir is
// always the EXECUTION view — the directory commands actually run in — so for
// docker it is the container-side /workspace constant, never the host mount
// source (that is the config's host_dir, a different concept).
func (h *SandboxHandler) annotate(cfg *store.SandboxConfig) {
	cfg.Terminal = terminalCapable(cfg)
	switch cfg.Type {
	case "ssh":
		var sc store.SSHConfig
		if len(cfg.Config) > 0 {
			_ = json.Unmarshal(cfg.Config, &sc)
		}
		// May be "" — but a session BINDING then requires an explicit directory
		// (ResolveBindingWorkDir): without a fixed dir, every exec runs in a
		// throw-away remote temp dir, which breaks session file continuity.
		cfg.DefaultWorkDir = sc.WorkDir
		cfg.WorkDirEditable = true
	case "local":
		cfg.DefaultWorkDir = h.workspaceAbs
		cfg.WorkDirEditable = true
	case "docker":
		// The mount point never moves, but a persistent container's session
		// may work in a /workspace subtree — so the directory is editable
		// within that constraint. An ephemeral container has no durable tree
		// to subdivide.
		cfg.DefaultWorkDir = bridge.DockerWorkspace
		var dc store.DockerConfig
		if len(cfg.Config) > 0 {
			_ = json.Unmarshal(cfg.Config, &dc)
		}
		cfg.WorkDirEditable = dc.Persistent
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
		h.annotate(&configs[i])
	}
	c.JSON(http.StatusOK, configs)
}

type createSandboxReq struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
	// Revision, on update only, is the revision the client's edit was based
	// on (from GET/List): editing from a stale form is then 409 instead of
	// silently overwriting a concurrent update. Absent (0) keeps
	// last-writer-wins. Ignored on create.
	Revision int64 `json:"revision,omitempty"`
}

func (r createSandboxReq) toConfig() *store.SandboxConfig {
	return &store.SandboxConfig{
		Name:   r.Name,
		Type:   r.Type,
		Config: r.Config,
	}
}

// validateSandbox enforces the POLICY layer of a sandbox write: name and
// type present, local gated behind its flag, remote docker daemons refused.
// Field-level validation and canonicalization live in
// store.NormalizeSandboxConfig, which both write handlers run right after
// this. The docker host check must stay HERE, before normalization: host is
// not a DockerConfig field, so the canonical re-marshal would silently drop
// it instead of telling the user it is unsupported.
func (h *SandboxHandler) validateSandbox(c *gin.Context, req *createSandboxReq) bool {
	if req.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	switch req.Type {
	case "local", "ssh":
		if req.Type == "local" && !h.allowLocalSandbox {
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
	canonical, err := store.NormalizeSandboxConfig(cfg.Type, cfg.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	cfg.Config = canonical
	if err := h.store.Create(c.Request.Context(), cfg); err != nil {
		internalError(c, err)
		return
	}
	created := sanitizeSandboxConfig(*cfg)
	h.annotate(&created)
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
	h.annotate(&out)
	c.JSON(http.StatusOK, out)
}

// Update overwrites the sandbox configuration identified by the id path
// parameter and responds with the updated configuration. A masked SSH
// password keeps the stored value. An update that would change the sandbox's
// identity (type, ssh addr/work_dir, docker host_dir/persistent/
// container_name) is refused with 409 while sessions are bound to it — their
// binding is permanent, and rewriting what the id points at would switch a
// conversation's file system under it. Non-identity fields update freely.
//
//	@Summary		Update sandbox
//	@Description	Include the revision the edit was based on (from GET/List) to make the write conditional: 409 if the config changed meanwhile. Omitting it falls back to last-writer-wins.
//	@Tags			sandboxes
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string				true	"Sandbox ID"
//	@Param			sandbox	body		createSandboxReq	true	"Sandbox configuration"
//	@Success		200		{object}	store.SandboxConfig
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse	"local sandbox disabled"
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"identity change refused (sessions are bound), or the config changed concurrently — re-read and retry"
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandboxes/{id} [put]
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
	// values, and so its revision anchors the CAS below. A transient
	// (non-not-found) Get failure must abort: continuing with an empty prev
	// would resolve the ******** mask to "" and silently WIPE the stored ssh
	// password — the same guard every sibling handler carries.
	prev, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	cfg := req.toConfig()
	cfg.Config = restoreSandboxConfig(cfg.Type, cfg.Config, prev.Config)
	// Normalize AFTER the mask restore: the canonical form must carry the
	// real secret, not the ******** sentinel.
	canonical, err := store.NormalizeSandboxConfig(cfg.Type, cfg.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	cfg.Config = canonical
	// Everything decided from prev — the identity comparison, contentChanged
	// — holds only while the row IS prev, which the expected-revision CAS on
	// both write paths guarantees: a concurrent update moves the revision,
	// this write refuses (409), the client re-reads. The anchor is the
	// client's own revision when the request names one, extending the
	// guarantee back to the form the edit was made on; without one it is
	// prev — concurrent handlers still serialize, clients are
	// last-writer-wins.
	expected := prev.Revision
	if req.Revision != 0 {
		expected = req.Revision
	}
	contentChanged := prev.Type != cfg.Type || !store.ContentEqual(cfg.Type, prev.Config, cfg.Config)
	if store.IdentityChanged(prev, cfg) {
		refs, uerr := h.store.UpdateIdentityIfUnreferenced(ctx, id, cfg, expected)
		if uerr != nil {
			storeError(c, uerr)
			return
		}
		if refs > 0 {
			conflict(c, fmt.Sprintf("%d session(s) are bound to this sandbox; its type, machine, directory and container are frozen — credentials, name and limits stay editable, or create a new sandbox for the new location", refs))
			return
		}
	} else if err := h.store.Update(ctx, id, cfg, expected, contentChanged); err != nil {
		storeError(c, err)
		return
	}
	// The write landed; invalidate NOW, from what the CAS guarantees
	// (revision moved to prev+1, the generation iff content changed) — not
	// from a re-read that a cancelled request could fail, leaving new
	// credentials in the store while old instances and terminals keep
	// serving. Only a CONTENT change retires: a rename must not sever
	// terminals or close idle persistent containers (docker's Close deletes
	// the container and its volumes).
	if contentChanged {
		h.manager.Retire(id, prev.RuntimeGen+1)
		h.closeSandboxTerminals(id, prev.RuntimeGen+1)
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	out := sanitizeSandboxConfig(*updated)
	h.annotate(&out)
	c.JSON(http.StatusOK, out)
}

// Delete removes the sandbox configuration identified by the id path parameter.
// A sandbox still referenced by session bindings is refused with 409: the
// binding is permanent, so deleting its target would leave those sessions
// failing every run with no way back. Delete the sessions first (or keep the
// sandbox). The refusal is decided by the delete statement itself
// (DeleteIfUnreferenced), not a prior count — a first-run bind racing this
// delete therefore either lands before it (and blocks it) or loses its own
// EXISTS predicate; a session can never end up bound to a config this removed.
//
//	@Summary	Delete sandbox
//	@Tags		sandboxes
//	@Param		id	path	string	true	"Sandbox ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"sessions are bound to this sandbox"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id} [delete]
func (h *SandboxHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	// Delete from the DB first: only tear down the live instance once the row
	// is gone, so a failed delete doesn't leave a persisted sandbox with its
	// running instance already closed.
	refs, err := h.store.DeleteIfUnreferenced(c.Request.Context(), id)
	if err != nil {
		storeError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, fmt.Sprintf("%d session(s) are bound to this sandbox; delete them first", refs))
		return
	}
	h.manager.Remove(id)
	// Every generation goes: the config no longer exists, so no terminal —
	// registered or still dialing — may keep serving it.
	h.closeSandboxTerminals(id, math.MaxInt64)
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

	sb, release, err := h.manager.Acquire(cfg, "")
	if err != nil {
		upstreamError(c, err)
		return
	}
	defer release()

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
