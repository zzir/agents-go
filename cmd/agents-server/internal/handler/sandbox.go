package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
)

// SandboxHandler serves CRUD endpoints and code execution for sandboxes.
type SandboxHandler struct {
	store     *store.SandboxStore
	manager   *sandboxes.Manager
	terminals *TerminalHandler
}

// NewSandboxHandler returns a handler over the sandbox store and manager;
// terminals is the web-terminal registry an update or delete tears down.
func NewSandboxHandler(s *store.SandboxStore, m *sandboxes.Manager, terminals *TerminalHandler) *SandboxHandler {
	if s == nil || terminals == nil {
		panic("handler: NewSandboxHandler needs the sandbox store and the terminal handler")
	}
	return &SandboxHandler{store: s, manager: m, terminals: terminals}
}

// closeSandboxTerminals tears down live web terminals opened under a config
// generation below minGen, and moves the registry's fence so a terminal still
// dialing cannot register afterwards.
func (h *SandboxHandler) closeSandboxTerminals(id string, minGen int64) {
	h.terminals.CloseSandboxTerminals(id, minGen)
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
// type present, docker the only backend (spec §5.27). Field-level validation
// and canonicalization live in store.NormalizeSandboxConfig, which both
// write handlers run right after this.
func (h *SandboxHandler) validateSandbox(c *gin.Context, req *createSandboxReq) bool {
	if req.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	switch req.Type {
	case "docker":
	case "":
		badRequest(c, "type is required")
		return false
	default:
		badRequest(c, "type must be docker, got "+req.Type)
		return false
	}
	return true
}

// Create persists a new sandbox configuration from the request body.
//
//	@Summary		Create sandbox
//	@Description	type is "docker". config: image (required), host ("" = local daemon, tcp://, or ssh://user@host with ssh_* auth — ssh_password is write-only, ******** mask semantics), runtime, user, network, memory_mb/cpus caps, max_read_file_bytes (0 = 8 MiB default).
//	@Tags			sandboxes
//	@Accept			json
//	@Produce		json
//	@Param			sandbox	body		createSandboxReq	true	"Sandbox configuration"
//	@Success		201		{object}	store.SandboxConfig
//	@Failure		400		{object}	ErrorResponse
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
	view := sanitizeSandboxConfig(*cfg)
	created(c, view.ID, view)
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
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"identity change refused (sessions or projects live on it), or the config changed concurrently — re-read and retry"
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
	// Only refuse when a stored password actually exists — a mask with
	// nothing behind it resolves to "" and needs no guard.
	if maskAcrossDestination(cfg.Config, prev.Config, "host") && storedSSHPassword(prev.Config) {
		badRequest(c, "host changed: the stored ssh_password belongs to the previous host — replace it or clear it")
		return
	}
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
		sessions, projects, uerr := h.store.UpdateIdentityIfUnreferenced(ctx, id, cfg, expected)
		if uerr != nil {
			storeError(c, uerr)
			return
		}
		if sessions+projects > 0 {
			conflict(c, fmt.Sprintf("%d session(s) and %d project(s) live on this sandbox; its type, machine, directory and container are frozen — credentials, name and limits stay editable, or create a new sandbox for the new location", sessions, projects))
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

	// The health check runs in a throw-away EPHEMERAL container, bypassing
	// the manager: a test needs no project tree, and must not leave a
	// persistent container behind.
	sb, err := h.testSandbox(cfg)
	if err != nil {
		upstreamError(c, err)
		return
	}
	defer func() { _ = sb.Close() }()

	timeout := sandbox.DefaultTimeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout+5*time.Second)
	defer cancel()

	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Cmd:     []string{"sh", "-c", "echo ok"},
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

// daemonOptions assembles the SDK options reaching cfg's daemon — shared by
// the ephemeral health check and the managed-container admin calls.
func daemonOptions(cfg *store.SandboxConfig) (dockersb.Options, error) {
	var dc store.DockerConfig
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &dc); err != nil {
			return dockersb.Options{}, fmt.Errorf("invalid config: %w", err)
		}
	}
	if dc.Image == "" {
		return dockersb.Options{}, fmt.Errorf("docker sandbox requires an image")
	}
	opts := dockersb.Options{
		Image:   dc.Image,
		Host:    dc.Host,
		Runtime: dc.Runtime,
		User:    dc.User,
		Network: dc.Network,
		Limits:  sandbox.Limits{MemoryBytes: dc.MemoryMB << 20, CPUs: dc.CPUs},
	}
	if strings.HasPrefix(dc.Host, "ssh://") {
		opts.SSH = dockersb.SSHAuth{
			UseAgent:              dc.SSHUseAgent,
			KeyFile:               dc.SSHKeyFile,
			Password:              dc.SSHPassword,
			KnownHostsFile:        dc.SSHKnownHosts,
			InsecureIgnoreHostKey: dc.SSHInsecureHostKey,
		}
	}
	return opts, nil
}

// testSandbox builds a throw-away ephemeral SDK sandbox from cfg — same
// image, daemon and limits as the real containers, no name, no mount.
func (h *SandboxHandler) testSandbox(cfg *store.SandboxConfig) (sandbox.Sandbox, error) {
	opts, err := daemonOptions(cfg)
	if err != nil {
		return nil, err
	}
	return dockersb.New(opts)
}

// Containers lists this package's containers on the sandbox's daemon —
// running and stopped, foreign containers never included.
//
//	@Summary	List the sandbox's managed containers
//	@Tags		sandboxes
//	@Produce	json
//	@Param		id	path		string	true	"Sandbox id"
//	@Success	200	{array}		dockersb.ManagedContainer
//	@Failure	404	{object}	ErrorResponse
//	@Failure	502	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id}/containers [get]
func (h *SandboxHandler) Containers(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	opts, err := daemonOptions(cfg)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	out, err := dockersb.ListManaged(c.Request.Context(), opts)
	if err != nil {
		upstreamError(c, err)
		return
	}
	if out == nil {
		out = []dockersb.ManagedContainer{}
	}
	c.JSON(http.StatusOK, out)
}

// containerAct resolves the sandbox and the (prefix-checked) container name
// for the stop/remove endpoints, answering the error when either fails.
func (h *SandboxHandler) containerAct(c *gin.Context) (dockersb.Options, string, bool) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return dockersb.Options{}, "", false
	}
	name := c.Param("name")
	// The SDK re-verifies ownership by label; the prefix check just refuses
	// obviously-foreign names before a daemon round-trip.
	if !strings.HasPrefix(name, "agents-") {
		badRequest(c, "not a managed container name")
		return dockersb.Options{}, "", false
	}
	opts, err := daemonOptions(cfg)
	if err != nil {
		badRequest(c, err.Error())
		return dockersb.Options{}, "", false
	}
	return opts, name, true
}

// StopContainer stops one managed container; the next run starts it again.
//
//	@Summary	Stop a managed container
//	@Tags		sandboxes
//	@Param		id		path	string	true	"Sandbox id"
//	@Param		name	path	string	true	"Container name (agents-…)"
//	@Success	204		"stopped"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	502		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id}/containers/{name}/stop [post]
func (h *SandboxHandler) StopContainer(c *gin.Context) {
	opts, name, ok := h.containerAct(c)
	if !ok {
		return
	}
	if err := dockersb.StopManaged(c.Request.Context(), opts, name); err != nil {
		upstreamError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// RemoveContainer force-removes one managed container — the rebuild act: the
// project's tree (bind mount or volume) survives, and the next run recreates
// the container from the current configuration.
//
//	@Summary	Remove a managed container (rebuild)
//	@Tags		sandboxes
//	@Param		id		path	string	true	"Sandbox id"
//	@Param		name	path	string	true	"Container name (agents-…)"
//	@Success	204		"removed"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	502		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id}/containers/{name} [delete]
func (h *SandboxHandler) RemoveContainer(c *gin.Context) {
	opts, name, ok := h.containerAct(c)
	if !ok {
		return
	}
	if err := dockersb.RemoveManaged(c.Request.Context(), opts, name); err != nil {
		upstreamError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// sandboxTestResp is the Test response: whether the health-check command
// succeeded, with failure detail when it didn't.
type sandboxTestResp struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}
