package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	e2bsb "github.com/zzir/agents-go/sandbox/e2b"
)

// WorkspaceHandler manages workspaces, the per-user working trees on a sandbox —
// see decisions §5.28; who may act on whose rows is decisions §5.29.
type WorkspaceHandler struct {
	// Audit, when set, records every working-tree export. Wired at bootstrap.
	Audit protocol.AuditFunc
	// Notes, when set, lets a rebuild leave its note on the session it was
	// asked from. Wired at bootstrap.
	Notes *RebuildNoteDeps
	// StopCommands, when set, ends the workspace's background commands before
	// its compute stops. Wired at bootstrap.
	StopCommands func(ctx context.Context, workspaceID string)
	store        *store.WorkspaceStore
	sandboxes    *store.SandboxStore
	manager      *sandboxes.Manager
	terminals    *TerminalHandler
	settings     *settings.Reader
}

// NewWorkspaceHandler returns a handler over the workspace store; m reclaims a
// deleted workspace's storage, and terminals is the registry a content change severs.
func NewWorkspaceHandler(s *store.WorkspaceStore, sbs *store.SandboxStore, m *sandboxes.Manager, terminals *TerminalHandler, cfg *settings.Reader) *WorkspaceHandler {
	return &WorkspaceHandler{store: s, sandboxes: sbs, manager: m, terminals: terminals, settings: cfg}
}

// workspaceDetail is the single-workspace response: the row plus its environment
// NAMES, every value masked. Env shadows the row's json:"-" field on purpose.
type workspaceDetail struct {
	store.Workspace
	Env []store.EnvVar `json:"env"`
}

// maskWorkspaceEnv replaces every value with the sentinel restoreWorkspaceEnv
// resolves back; names stay readable — decisions §5.32.
func maskWorkspaceEnv(vars []store.EnvVar) []store.EnvVar {
	out := make([]store.EnvVar, 0, len(vars))
	for _, v := range vars {
		v.Value = maskSecret(v.Value) // an empty value stays empty, as everywhere else
		out = append(out, v)
	}
	return out
}

// restoreWorkspaceEnv resolves masked values against the stored ones BY NAME,
// so a mask never rides to a name it was not stored under — invariant 9.
func restoreWorkspaceEnv(incoming, prev []store.EnvVar) ([]store.EnvVar, error) {
	stored := make(map[string]string, len(prev))
	for _, v := range prev {
		stored[v.Key] = v.Value
	}
	out := make([]store.EnvVar, 0, len(incoming))
	for _, v := range incoming {
		if v.Value == SecretMask {
			old, ok := stored[v.Key]
			if !ok {
				return nil, fmt.Errorf("%q was sent masked but has no stored value; send its value or remove it", v.Key)
			}
			v.Value = old
		}
		out = append(out, v)
	}
	return out, nil
}

// detail builds the single-workspace response, masking the environment. An
// undecodable stored payload is reported rather than silently shown empty.
func (h *WorkspaceHandler) detail(p *store.Workspace) (*workspaceDetail, error) {
	vars, err := store.DecodeWorkspaceEnv(p.Env)
	if err != nil {
		return nil, err
	}
	out := &workspaceDetail{Workspace: *p, Env: maskWorkspaceEnv(vars)}
	return out, nil
}

// workspaceDeleteResp answers a delete. The row is always gone when this is
// returned; storage_error names storage that could not be reclaimed with it.
type workspaceDeleteResp struct {
	Deleted      bool   `json:"deleted"`
	StorageError string `json:"storage_error,omitempty"`
}

// own resolves the caller's workspace by id; an admin's reach does not extend
// here, and a foreign workspace reads as absent.
func (h *WorkspaceHandler) own(c *gin.Context) (*store.Workspace, bool) {
	ownerID, _, ok := callerScope(c)
	if !ok {
		return nil, false
	}
	p, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return nil, false
	}
	if p.OwnerID != ownerID {
		notFound(c)
		return nil, false
	}
	return p, true
}

// manage resolves the workspace for an operation on its COMPUTE (status, start,
// stop, rebuild); an admin passes on any. Export, which discloses files, uses own.
func (h *WorkspaceHandler) manage(c *gin.Context) (*store.Workspace, bool) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return nil, false
	}
	p, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return nil, false
	}
	if !admin && p.OwnerID != ownerID {
		notFound(c)
		return nil, false
	}
	return p, true
}

// List responds with the caller's workspaces; `?all=true` is the admin's
// listing of every owner's.
//
//	@Summary		List workspaces
//	@Description	Every row carries session_count. storage_hint (where the files live) is reported to admins only.
//	@Tags			workspaces
//	@Produce		json
//	@Param			all	query		bool	false	"admin: every owner's workspaces"
//	@Success		200	{array}		store.Workspace
//	@Failure		403	{object}	ErrorResponse	"all=true by a member"
//	@Security		BearerAuth
//	@Router			/workspaces [get]
func (h *WorkspaceHandler) List(c *gin.Context) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	owner := ownerID
	if c.Query("all") == "true" {
		if !requireAdmin(c) {
			return
		}
		owner = store.EveryOwner
	}
	out, err := h.store.List(c.Request.Context(), owner)
	if err != nil {
		storeError(c, err)
		return
	}
	if out == nil {
		out = []store.Workspace{}
	}
	hosts := map[string]string{}
	for i := range out {
		if admin {
			out[i].StorageHint = h.storageHint(c, hosts, &out[i])
		}
	}
	c.JSON(http.StatusOK, out)
}

// storageHint names where p's files live (decisions §5.33); admin-only, as a
// daemon address is a server-side fact. hints caches the per-sandbox half.
func (h *WorkspaceHandler) storageHint(c *gin.Context, hints map[string]string, p *store.Workspace) string {
	where, ok := hints[p.SandboxID]
	if !ok {
		sb, err := h.sandboxes.Get(c.Request.Context(), p.SandboxID)
		if err != nil {
			return ""
		}
		where = store.SandboxStorageWhere(sb)
		hints[p.SandboxID] = where
	}
	if strings.HasPrefix(where, "sandbox on ") {
		ref := p.InstanceRef
		if ref == "" {
			ref = "not provisioned yet"
		}
		return ref + " — a " + where
	}
	return "docker volume " + sandboxes.WorkspaceVolumeName(p.ID) + " on " + where
}

type workspaceReq struct {
	Name string `json:"name" binding:"required"`
	// SandboxID is what the workspace runs on — the machine and the image.
	SandboxID string `json:"sandbox_id" binding:"required"`
	// Env is the environment the workspace's container is created with;
	// optional, and empty means none.
	Env []store.EnvVar `json:"env,omitempty"`
}

// workspaceUpdateReq is the update body: the name, the sandbox, the whole
// environment, and the revision the edit was made against (optional).
type workspaceUpdateReq struct {
	Name      string         `json:"name" binding:"required"`
	SandboxID string         `json:"sandbox_id" binding:"required"`
	Env       []store.EnvVar `json:"env,omitempty"`
	Revision  int64          `json:"revision,omitempty"`
}

// Create makes a new workspace for the caller on the named sandbox.
//
//	@Summary	Create workspace
//	@Tags		workspaces
//	@Accept		json
//	@Produce	json
//	@Param		workspace	body		workspaceReq	true	"Name and sandbox"
//	@Success	201			{object}	workspaceDetail
//	@Failure	400			{object}	ErrorResponse
//	@Failure	409			{object}	ErrorResponse	"name already in use on this sandbox"
//	@Security	BearerAuth
//	@Router		/workspaces [post]
func (h *WorkspaceHandler) Create(c *gin.Context) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	var req workspaceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "name and sandbox_id are required")
		return
	}
	env, err := store.NormalizeWorkspaceEnv(req.Env)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	// A missing sandbox 404s from the insert itself: the store locks the row.
	p := &store.Workspace{OwnerID: ownerID, SandboxID: req.SandboxID, Name: req.Name, Env: env}
	if err := h.store.Create(c.Request.Context(), p); err != nil {
		saveError(c, err)
		return
	}
	if admin {
		p.StorageHint = h.storageHint(c, map[string]string{}, p)
	}
	out, err := h.detail(p)
	if err != nil {
		internalError(c, err)
		return
	}
	created(c, p.ID, out)
}

// Get responds with one of the caller's workspaces, environment masked.
//
//	@Summary		Get workspace
//	@Description	The one endpoint that returns a workspace's environment — names, with every value masked. Listings never carry it at all. Owner only: an environment is not part of an admin's management reach.
//	@Tags			workspaces
//	@Produce		json
//	@Param			id	path		string	true	"Workspace id"
//	@Success		200	{object}	workspaceDetail
//	@Failure		404	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workspaces/{id} [get]
func (h *WorkspaceHandler) Get(c *gin.Context) {
	p, ok := h.own(c)
	if !ok {
		return
	}
	out, err := h.detail(p)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// Update renames the caller's workspace and replaces its environment.
//
//	@Summary		Update workspace
//	@Description	A value sent back as its mask keeps what is stored; any other value replaces it. An environment change replaces the workspace's container at its next run and severs its terminals; a rename does none of that.
//	@Tags			workspaces
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string				true	"Workspace id"
//	@Param			workspace	body		workspaceUpdateReq	true	"Name and environment"
//	@Success		200			{object}	workspaceDetail
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"name already in use, or the workspace changed concurrently — re-read and retry"
//	@Security		BearerAuth
//	@Router			/workspaces/{id} [put]
func (h *WorkspaceHandler) Update(c *gin.Context) {
	var req workspaceUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "name and sandbox_id are required")
		return
	}
	prev, ok := h.own(c)
	if !ok {
		return
	}
	stored, err := store.DecodeWorkspaceEnv(prev.Env)
	if err != nil {
		internalError(c, err)
		return
	}
	// Masks resolve before normalization: the canonical payload carries real values.
	restored, err := restoreWorkspaceEnv(req.Env, stored)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	env, err := store.NormalizeWorkspaceEnv(restored)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	// Revision CAS, anchored on the client's revision when given: a race is 409.
	expected := prev.Revision
	if req.Revision != 0 {
		expected = req.Revision
	}
	contentChanged := !store.EnvContentEqual(prev.Env, env) ||
		req.SandboxID != prev.SandboxID
	next := *prev
	next.Name, next.SandboxID, next.Env = req.Name, req.SandboxID, env
	newGen, err := h.store.Update(c.Request.Context(), prev.ID, &next, expected, contentChanged)
	if err != nil {
		saveError(c, err)
		return
	}
	// Invalidate from the generation the store wrote, not prev+1 or a re-read.
	if contentChanged {
		h.manager.RetireWorkspace(prev.ID, newGen)
		h.terminals.CloseWorkspaceTerminals(prev.ID, newGen)
	}
	// Re-read for the response, so the client holds the current revision.
	updated, err := h.store.Get(c.Request.Context(), prev.ID)
	if err != nil {
		storeError(c, err)
		return
	}
	out, err := h.detail(updated)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// RebuildNoteDeps is what a rebuild needs to leave its note on a session.
type RebuildNoteDeps struct {
	Sessions *store.SessionStore
	Entries  *store.EntryStore
	Fence    RunStopper
}

// rebuildReq is RebuildContainer's optional body.
type rebuildReq struct {
	// SessionID is the caller's session, bound to this workspace, to leave the
	// rebuilt note on; left out, no note is written.
	SessionID string `json:"session_id,omitempty"`
}

// RebuildContainer discards the workspace's container and creates a fresh one.
//
//	@Summary		Rebuild the workspace's sandbox
//	@Description	Discards the container and creates a fresh one from the current sandbox and environment. Files under /workspace survive; anything installed into the container does not, and commands running in it fail. Synchronous. Owner or admin. Refused on a sandbox whose instance IS the storage (E2B-compatible): export first. With a body naming session_id — the caller's own session, bound to this workspace and with no run live on it — the session gets a note that the container was rebuilt; otherwise none is written and the rebuild still succeeds.
//	@Tags			workspaces
//	@Accept			json
//	@Param			id		path	string		true	"Workspace id"
//	@Param			body	body	rebuildReq	false	"The session to note the rebuild on"
//	@Success		204		"rebuilt"
//	@Failure		404		{object}	ErrorResponse
//	@Failure		502		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workspaces/{id}/sandbox/rebuild [post]
func (h *WorkspaceHandler) RebuildContainer(c *gin.Context) {
	var req rebuildReq
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			badRequest(c, err.Error())
			return
		}
	}
	p, ok := h.manage(c)
	if !ok {
		return
	}
	spec, err := resolveSpec(c.Request.Context(), h.sandboxes, p)
	if err != nil {
		storeError(c, err)
		return
	}
	if err := h.manager.RebuildContainer(c.Request.Context(), spec); err != nil {
		upstreamError(c, err)
		return
	}
	if req.SessionID != "" {
		h.noteRebuild(c, p, req.SessionID)
	}
	c.Status(http.StatusNoContent)
}

// noteRebuild leaves the rebuilt note on the session when it is the caller's,
// bound to the workspace and at rest; otherwise nothing, and never a failure.
func (h *WorkspaceHandler) noteRebuild(c *gin.Context, p *store.Workspace, sessionID string) {
	if h.Notes == nil {
		return
	}
	ownerID, _, ok := callerScope(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	sess, err := h.Notes.Sessions.Get(ctx, sessionID)
	if err != nil || sess.OwnerID != ownerID || sess.WorkspaceID != p.ID {
		return
	}
	err = h.Notes.Fence.WithSessionTreeFenced(ctx, sessionID, func() error {
		ref, err := h.Notes.Entries.RefFor(ctx, sessionID)
		if err != nil {
			return err
		}
		return h.Notes.Entries.AppendContainerRebuilt(ctx, ref, store.ContainerRebuilt{WorkspaceID: p.ID, WorkspaceName: p.Name})
	})
	if err != nil {
		if _, busy := errors.AsType[bridge.ErrSessionBusy](err); !busy {
			logging.Ctx(ctx).Warn("recording the container-rebuilt note", "error", err, "session_id", sessionID)
		}
	}
}

// sandboxStateResp is the workspace's compute state, as the UI badge shows it.
type sandboxStateResp struct {
	// State is absent | stopped | running.
	State string `json:"state"`
}

// sandboxStopResp says whether the sandbox stopped now or stops when the work
// using it finishes.
type sandboxStopResp struct {
	Stopped bool `json:"stopped"`
}

// SandboxStatus reports what the workspace's compute is doing.
//
//	@Summary	Workspace sandbox status
//	@Tags		workspaces
//	@Produce	json
//	@Param		id	path		string	true	"Workspace id"
//	@Success	200	{object}	sandboxStateResp
//	@Failure	404	{object}	ErrorResponse
//	@Failure	502	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/workspaces/{id}/sandbox [get]
func (h *WorkspaceHandler) SandboxStatus(c *gin.Context) {
	spec, ok := h.spec(c)
	if !ok {
		return
	}
	state, err := h.manager.Status(c.Request.Context(), spec)
	if err != nil {
		upstreamError(c, err)
		return
	}
	c.JSON(http.StatusOK, sandboxStateResp{State: state.String()})
}

// workspaceHostResp is where a port inside the workspace's sandbox is public: https://<port>-<sandbox_id>.<domain>.
type workspaceHostResp struct {
	SandboxID string `json:"sandbox_id"`
	Domain    string `json:"domain"`
}

// SandboxHost names where the workspace's sandbox serves its ports.
//
//	@Summary		Workspace sandbox public address
//	@Description	The sandbox id and the domain a port inside the sandbox is public at, as https://<port>-<sandbox_id>.<domain>. Owner only. 409 where the sandbox's row does not declare supports.public_host, or where there is no sandbox to address (none provisioned yet, or gone).
//	@Tags			workspaces
//	@Produce		json
//	@Param			id	path		string	true	"Workspace id"
//	@Success		200	{object}	workspaceHostResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"ports not public, or no sandbox to address"
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workspaces/{id}/host [get]
func (h *WorkspaceHandler) SandboxHost(c *gin.Context) {
	spec, ok := h.ownSpec(c)
	if !ok {
		return
	}
	if !store.SandboxSupportsFor(spec.Sandbox.Type).PublicHost {
		conflict(c, "this sandbox's ports are not public")
		return
	}
	id, domain, err := h.manager.Address(c.Request.Context(), spec)
	switch {
	case errors.Is(err, e2bsb.ErrNoSandbox):
		conflict(c, "no sandbox to address — one is created at the first run, or by Start")
		return
	case err != nil:
		upstreamError(c, err)
		return
	}
	if h.Audit != nil {
		user, _ := server.CurrentUser(c)
		h.Audit(context.WithoutCancel(c.Request.Context()), protocol.AuditRecord{
			Actor: user, Action: "workspace.host", Resource: spec.Workspace.ID,
			Detail: "workspace " + spec.Workspace.Name,
		})
	}
	c.JSON(http.StatusOK, workspaceHostResp{SandboxID: id, Domain: domain})
}

// SandboxStart provisions the workspace's sandbox and makes it ready, image
// pull included.
//
//	@Summary		Start the workspace's sandbox
//	@Description	Synchronous, and can take an image pull's worth of time. Owner or admin.
//	@Tags			workspaces
//	@Param			id	path	string	true	"Workspace id"
//	@Success		204	"running"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workspaces/{id}/sandbox/start [post]
func (h *WorkspaceHandler) SandboxStart(c *gin.Context) {
	h.containerAct(c, h.manager.EnsureRunning)
}

// SandboxStop releases the compute, keeping the working tree.
//
//	@Summary		Stop the workspace's sandbox
//	@Description	Keeps the working tree. `stopped: false` means a run or terminal is still using it and the stop happens when that ends. Owner or admin.
//	@Tags			workspaces
//	@Produce		json
//	@Param			id	path		string	true	"Workspace id"
//	@Success		200	{object}	sandboxStopResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workspaces/{id}/sandbox/stop [post]
func (h *WorkspaceHandler) SandboxStop(c *gin.Context) {
	spec, ok := h.spec(c)
	if !ok {
		return
	}
	// A command still running would hold the compute: it is ended first.
	if h.StopCommands != nil {
		h.StopCommands(c.Request.Context(), spec.Workspace.ID)
	}
	stopped, err := h.manager.Stop(c.Request.Context(), spec)
	if err != nil {
		upstreamError(c, err)
		return
	}
	c.JSON(http.StatusOK, sandboxStopResp{Stopped: stopped})
}

// Export streams the workspace's working tree as a tar archive — decisions §5.33.
//
//	@Summary		Export the workspace's working tree
//	@Description	Streams /workspace as an uncompressed tar. Owner only, and audited: this takes the whole tree off the machine.
//	@Tags			workspaces
//	@Produce		application/x-tar
//	@Param			id	path	string	true	"Workspace id"
//	@Success		200	"tar stream"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workspaces/{id}/export [get]
func (h *WorkspaceHandler) Export(c *gin.Context) {
	// Owner only, unlike the lifecycle routes.
	spec, ok := h.ownSpec(c)
	if !ok {
		return
	}
	rc, err := h.manager.ExportWorkspace(c.Request.Context(), spec)
	if err != nil {
		upstreamError(c, err)
		return
	}
	defer func() { _ = rc.Close() }()
	if h.Audit != nil {
		user, _ := server.CurrentUser(c)
		h.Audit(context.WithoutCancel(c.Request.Context()), protocol.AuditRecord{
			Actor: user, Action: "workspace.export", Resource: spec.Workspace.ID,
			Detail: "workspace " + spec.Workspace.Name,
		})
	}
	// Headers go out before the first byte: a mid-stream failure shows as a
	// truncated archive.
	c.Header("Content-Disposition", `attachment; filename="`+tarFilename(spec.Workspace.Name)+`"`)
	c.Header("Content-Type", "application/x-tar")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, rc); err != nil {
		logging.Ctx(c.Request.Context()).Warn("workspace export stream ended early", "error", err, "workspace_id", spec.Workspace.ID)
	}
}

// tarFilename turns a workspace name into a safe download name.
func tarFilename(name string) string {
	out := make([]rune, 0, len(name)+4)
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "workspace.tar"
	}
	return string(out) + ".tar"
}

// spec resolves the caller's workspace into a build spec, answering the error
// when it cannot; owner or admin (see manage).
func (h *WorkspaceHandler) spec(c *gin.Context) (sandboxes.Spec, bool) {
	return h.specFrom(c, h.manage)
}

// ownSpec is spec for the routes that reach into the sandbox: owner only.
func (h *WorkspaceHandler) ownSpec(c *gin.Context) (sandboxes.Spec, bool) {
	return h.specFrom(c, h.own)
}

func (h *WorkspaceHandler) specFrom(c *gin.Context, resolve func(*gin.Context) (*store.Workspace, bool)) (sandboxes.Spec, bool) {
	p, ok := resolve(c)
	if !ok {
		return sandboxes.Spec{}, false
	}
	spec, err := resolveSpec(c.Request.Context(), h.sandboxes, p)
	if err != nil {
		storeError(c, err)
		return sandboxes.Spec{}, false
	}
	return spec, true
}

// containerAct resolves the workspace into a build spec, then runs one of the
// manager's sandbox calls against it. Owner or admin (see manage).
func (h *WorkspaceHandler) containerAct(c *gin.Context, act func(context.Context, sandboxes.Spec) error) {
	spec, ok := h.spec(c)
	if !ok {
		return
	}
	if err := act(c.Request.Context(), spec); err != nil {
		upstreamError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Delete removes the workspace and destroys its storage — decisions §5.33.
//
//	@Summary		Delete workspace
//	@Description	Deletes the working tree too — the container and its volume are removed. The owner deletes their own; an admin deletes any. The row is gone whenever this answers 200: a storage_error means the STORAGE could not be reclaimed and is left for the operator, not that the workspace survived.
//	@Tags			workspaces
//	@Param			id	path		string	true	"Workspace id"
//	@Success		200	{object}	workspaceDeleteResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"sessions still bound"
//	@Security		BearerAuth
//	@Router			/workspaces/{id} [delete]
func (h *WorkspaceHandler) Delete(c *gin.Context) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	p, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if p.OwnerID != ownerID && !admin {
		notFound(c) // a foreign workspace reads as absent
		return
	}
	refs, err := h.store.DeleteIfUnreferenced(c.Request.Context(), p.ID)
	if err != nil {
		storeError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, "sessions are still bound to this workspace; delete them first")
		return
	}
	// The workspace's shells die with it.
	h.terminals.CloseWorkspaceTerminals(p.ID, maxTerminalGen)
	// Reclaim the storage; a failure is reported inside the 200 — invariant 46.
	if h.manager != nil {
		// WithoutCancel: a client disconnect must not abort the reclaim.
		reclaimCtx := context.WithoutCancel(c.Request.Context())
		spec, serr := resolveSpec(reclaimCtx, h.sandboxes, p)
		if serr == nil {
			serr = h.manager.ReclaimWorkspace(reclaimCtx, spec)
		} else {
			h.manager.RemoveWorkspace(p.ID)
		}
		if serr != nil {
			logging.Ctx(c.Request.Context()).Warn("reclaiming a deleted workspace's storage", "workspace", p.ID, "error", serr)
			c.JSON(http.StatusOK, workspaceDeleteResp{Deleted: true, StorageError: serr.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, workspaceDeleteResp{Deleted: true})
}
