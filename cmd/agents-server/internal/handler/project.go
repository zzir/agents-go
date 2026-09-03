package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProjectHandler manages projects — per-user working trees on a sandbox
// (decisions §5.28). Routes act on the caller's own rows; an admin also lists
// every owner's (?all=true) and may delete any (decisions §5.29).
type ProjectHandler struct {
	// Audit, when set, records every working-tree export: the one call that
	// takes a whole project off the machine. Wired at bootstrap.
	Audit     protocol.AuditFunc
	store     *store.ProjectStore
	sandboxes *store.SandboxStore
	manager   *sandboxes.Manager
	terminals *TerminalHandler
	settings  *settings.Reader
}

// NewProjectHandler returns a handler over the project store; m reclaims a
// deleted project's storage, and terminals is the registry a content change severs.
func NewProjectHandler(s *store.ProjectStore, sbs *store.SandboxStore, m *sandboxes.Manager, terminals *TerminalHandler, cfg *settings.Reader) *ProjectHandler {
	return &ProjectHandler{store: s, sandboxes: sbs, manager: m, terminals: terminals, settings: cfg}
}

// projectDetail is the single-project response: the row plus its environment
// NAMES, every value masked. Env shadows the row's json:"-" field on purpose.
type projectDetail struct {
	store.Project
	Env []store.EnvVar `json:"env"`
}

// maskProjectEnv replaces every value with the sentinel restoreProjectEnv
// resolves back; names stay readable — decisions §5.32.
func maskProjectEnv(vars []store.EnvVar) []store.EnvVar {
	out := make([]store.EnvVar, 0, len(vars))
	for _, v := range vars {
		v.Value = maskSecret(v.Value) // an empty value stays empty, as everywhere else
		out = append(out, v)
	}
	return out
}

// restoreProjectEnv resolves masked values against the stored ones BY NAME,
// so a mask never rides to a name it was not stored under — invariant 9.
func restoreProjectEnv(incoming, prev []store.EnvVar) ([]store.EnvVar, error) {
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

// detail builds the single-project response, masking the environment. An
// undecodable stored payload is reported rather than silently shown empty.
func (h *ProjectHandler) detail(p *store.Project) (*projectDetail, error) {
	vars, err := store.DecodeProjectEnv(p.Env)
	if err != nil {
		return nil, err
	}
	out := &projectDetail{Project: *p, Env: maskProjectEnv(vars)}
	return out, nil
}

// projectDeleteResp answers a delete. The row is always gone when this is
// returned; storage_error names storage that could not be reclaimed with it.
type projectDeleteResp struct {
	Deleted      bool   `json:"deleted"`
	StorageError string `json:"storage_error,omitempty"`
}

// own resolves the caller's project by id; an admin's reach does NOT extend
// here (the environment is the owner's), and a foreign project reads as absent.
func (h *ProjectHandler) own(c *gin.Context) (*store.Project, bool) {
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

// manage resolves the project for an operation on its COMPUTE (status, start,
// stop, rebuild); an admin passes on any. Export, which discloses files, uses own.
func (h *ProjectHandler) manage(c *gin.Context) (*store.Project, bool) {
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

// List responds with the caller's projects; `?all=true` is the admin's
// listing of every owner's.
//
//	@Summary		List projects
//	@Description	Every row carries session_count. storage_hint (where the files live) is reported to admins only.
//	@Tags			projects
//	@Produce		json
//	@Param			all	query		bool	false	"admin: every owner's projects"
//	@Success		200	{array}		store.Project
//	@Failure		403	{object}	ErrorResponse	"all=true by a member"
//	@Security		BearerAuth
//	@Router			/projects [get]
func (h *ProjectHandler) List(c *gin.Context) {
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
		out = []store.Project{}
	}
	hosts := map[string]string{}
	for i := range out {
		if admin {
			out[i].StorageHint = h.storageHint(c, hosts, &out[i])
		}
	}
	c.JSON(http.StatusOK, out)
}

// storageHint names WHERE p's files live (decisions §5.33); admin-only, as a
// daemon address is a server-side fact. hints caches the per-sandbox half.
func (h *ProjectHandler) storageHint(c *gin.Context, hints map[string]string, p *store.Project) string {
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
	return "docker volume " + sandboxes.ProjectVolumeName(p.ID) + " on " + where
}

type projectReq struct {
	Name string `json:"name" binding:"required"`
	// SandboxID is what the project runs on — the machine and the image.
	SandboxID string `json:"sandbox_id" binding:"required"`
	// Env is the environment the project's container is created with;
	// optional, and empty means none.
	Env []store.EnvVar `json:"env,omitempty"`
}

// projectUpdateReq is the update body: the name, the sandbox, the whole
// environment, and the revision the edit was made against (optional).
type projectUpdateReq struct {
	Name      string         `json:"name" binding:"required"`
	SandboxID string         `json:"sandbox_id" binding:"required"`
	Env       []store.EnvVar `json:"env,omitempty"`
	Revision  int64          `json:"revision,omitempty"`
}

// Create makes a new project for the caller on the named sandbox.
//
//	@Summary	Create project
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		project	body		projectReq	true	"Name and sandbox"
//	@Success	201		{object}	projectDetail
//	@Failure	400		{object}	ErrorResponse
//	@Failure	409		{object}	ErrorResponse	"name already in use on this sandbox"
//	@Security	BearerAuth
//	@Router		/projects [post]
func (h *ProjectHandler) Create(c *gin.Context) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	var req projectReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "name and sandbox_id are required")
		return
	}
	env, err := store.NormalizeProjectEnv(req.Env)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	// Existence is the create's own guard (the store locks the row), so a
	// missing sandbox 404s from the insert itself.
	p := &store.Project{OwnerID: ownerID, SandboxID: req.SandboxID, Name: req.Name, Env: env}
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

// Get responds with one of the caller's projects and the names of its
// environment, every value masked.
//
//	@Summary		Get project
//	@Description	The one endpoint that returns a project's environment — names, with every value masked. Listings never carry it at all. Owner only: an environment is not part of an admin's management reach.
//	@Tags			projects
//	@Produce		json
//	@Param			id	path		string	true	"Project id"
//	@Success		200	{object}	projectDetail
//	@Failure		404	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id} [get]
func (h *ProjectHandler) Get(c *gin.Context) {
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

// Update renames the caller's project and replaces its environment.
//
//	@Summary		Update project
//	@Description	A value sent back as its mask keeps what is stored; any other value replaces it. An environment change replaces the project's container at its next run and severs its terminals; a rename does none of that.
//	@Tags			projects
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string				true	"Project id"
//	@Param			project	body		projectUpdateReq	true	"Name and environment"
//	@Success		200		{object}	projectDetail
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"name already in use, or the project changed concurrently — re-read and retry"
//	@Security		BearerAuth
//	@Router			/projects/{id} [put]
func (h *ProjectHandler) Update(c *gin.Context) {
	var req projectUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "name and sandbox_id are required")
		return
	}
	prev, ok := h.own(c)
	if !ok {
		return
	}
	stored, err := store.DecodeProjectEnv(prev.Env)
	if err != nil {
		internalError(c, err)
		return
	}
	// Masks resolve BEFORE normalization, so the canonical payload carries
	// real values rather than the sentinel.
	restored, err := restoreProjectEnv(req.Env, stored)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	env, err := store.NormalizeProjectEnv(restored)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	// Everything decided from prev holds only while the row IS prev: the
	// revision CAS (anchored on the client's revision when given) refuses a race with 409.
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
	// Invalidate from the generation the store wrote — not prev+1 (a racing
	// sandbox bump leaves it short), not a re-read a cancelled request could fail.
	if contentChanged {
		h.manager.RetireProject(prev.ID, newGen)
		h.terminals.CloseProjectTerminals(prev.ID, newGen)
	}
	// Re-read for the response: a client answering with a stale revision
	// would have its next update refused as a conflict.
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

// RebuildContainer discards the project's container and creates a fresh one.
//
//	@Summary		Rebuild the project's sandbox
//	@Description	Discards the container and creates a fresh one from the current sandbox and environment. Files under /workspace survive; anything installed into the container does not, and commands running in it fail. Synchronous. Owner or admin. Refused on a sandbox whose instance IS the storage (E2B-compatible): export first.
//	@Tags			projects
//	@Param			id	path	string	true	"Project id"
//	@Success		204	"rebuilt"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id}/sandbox/rebuild [post]
func (h *ProjectHandler) RebuildContainer(c *gin.Context) {
	h.containerAct(c, h.manager.RebuildContainer)
}

// sandboxStateResp is the project's compute state, as the UI badge shows it.
type sandboxStateResp struct {
	// State is absent | stopped | running.
	State string `json:"state"`
}

// sandboxStopResp says whether the sandbox stopped now or will stop when the
// work using it finishes — the honest answer to a Stop pressed mid-run.
type sandboxStopResp struct {
	Stopped bool `json:"stopped"`
}

// SandboxStatus reports what the project's compute is doing.
//
//	@Summary	Project sandbox status
//	@Tags		projects
//	@Produce	json
//	@Param		id	path		string	true	"Project id"
//	@Success	200	{object}	sandboxStateResp
//	@Failure	404	{object}	ErrorResponse
//	@Failure	502	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/projects/{id}/sandbox [get]
func (h *ProjectHandler) SandboxStatus(c *gin.Context) {
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

// SandboxStart provisions the project's sandbox and makes it ready — the
// image pull happens here, where a person is watching, instead of inside the
// next run.
//
//	@Summary		Start the project's sandbox
//	@Description	Synchronous, and can take an image pull's worth of time. Owner or admin.
//	@Tags			projects
//	@Param			id	path	string	true	"Project id"
//	@Success		204	"running"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id}/sandbox/start [post]
func (h *ProjectHandler) SandboxStart(c *gin.Context) {
	h.containerAct(c, h.manager.EnsureRunning)
}

// SandboxStop releases the compute, keeping the working tree. A run or an
// open terminal is not torn off its container: the response says the stop is
// deferred to whenever that finishes.
//
//	@Summary		Stop the project's sandbox
//	@Description	Keeps the working tree. `stopped: false` means a run or terminal is still using it and the stop happens when that ends. Owner or admin.
//	@Tags			projects
//	@Produce		json
//	@Param			id	path		string	true	"Project id"
//	@Success		200	{object}	sandboxStopResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id}/sandbox/stop [post]
func (h *ProjectHandler) SandboxStop(c *gin.Context) {
	spec, ok := h.spec(c)
	if !ok {
		return
	}
	stopped, err := h.manager.Stop(c.Request.Context(), spec)
	if err != nil {
		upstreamError(c, err)
		return
	}
	c.JSON(http.StatusOK, sandboxStopResp{Stopped: stopped})
}

// Export streams the project's working tree as a tar archive — the way files
// leave a sandbox whose storage the host cannot open directly
// (decisions §5.33). Owner only, like the environment: a tree is the owner's,
// and an admin's management reach does not extend to reading one.
//
//	@Summary		Export the project's working tree
//	@Description	Streams /workspace as an uncompressed tar. Owner only, and audited: this takes the whole tree off the machine.
//	@Tags			projects
//	@Produce		application/x-tar
//	@Param			id	path	string	true	"Project id"
//	@Success		200	"tar stream"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id}/export [get]
func (h *ProjectHandler) Export(c *gin.Context) {
	// Owner only, unlike the lifecycle routes: this hands over the whole
	// working tree, and managing the plane is not reading someone's files.
	p, ok := h.own(c)
	if !ok {
		return
	}
	spec, err := resolveSpec(c.Request.Context(), h.sandboxes, p)
	if err != nil {
		storeError(c, err)
		return
	}
	rc, err := h.manager.ExportProject(c.Request.Context(), spec)
	if err != nil {
		upstreamError(c, err)
		return
	}
	defer func() { _ = rc.Close() }()
	if h.Audit != nil {
		user, _ := server.CurrentUser(c)
		h.Audit(context.WithoutCancel(c.Request.Context()), protocol.AuditRecord{
			Actor: user, Action: "project.export", Resource: spec.Project.ID,
			Detail: "project " + spec.Project.Name,
		})
	}
	// The headers go out before the first byte: a failure mid-stream shows as
	// a truncated archive, which tar itself reports.
	c.Header("Content-Disposition", `attachment; filename="`+tarFilename(spec.Project.Name)+`"`)
	c.Header("Content-Type", "application/x-tar")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, rc); err != nil {
		logging.Ctx(c.Request.Context()).Warn("project export stream ended early", "error", err, "project_id", spec.Project.ID)
	}
}

// tarFilename turns a project name into a safe download name.
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
		return "project.tar"
	}
	return string(out) + ".tar"
}

// spec resolves the caller's project into a build spec, answering the error
// when it cannot.
func (h *ProjectHandler) spec(c *gin.Context) (sandboxes.Spec, bool) {
	p, ok := h.manage(c)
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

// containerAct resolves the project into a build spec, then runs one of the
// manager's sandbox calls against it. Owner or admin (see manage).
func (h *ProjectHandler) containerAct(c *gin.Context, act func(context.Context, sandboxes.Spec) error) {
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

// Delete removes the caller's project — an admin's: any project — while no
// session binds it, then DESTROYS its storage: the container and the volume
// holding the working tree (decisions §5.33).
//
//	@Summary		Delete project
//	@Description	Deletes the working tree too — the container and its volume are removed. The owner deletes their own; an admin deletes any. The row is gone whenever this answers 200: a storage_error means the STORAGE could not be reclaimed and is left for the operator, not that the project survived.
//	@Tags			projects
//	@Param			id	path		string	true	"Project id"
//	@Success		200	{object}	projectDeleteResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"sessions still bound"
//	@Security		BearerAuth
//	@Router			/projects/{id} [delete]
func (h *ProjectHandler) Delete(c *gin.Context) {
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
		notFound(c) // a foreign project reads as absent
		return
	}
	refs, err := h.store.DeleteIfUnreferenced(c.Request.Context(), p.ID)
	if err != nil {
		storeError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, "sessions are still bound to this project; delete them first")
		return
	}
	// The project is gone: its shells must die with it — nothing may keep
	// serving a tree that is about to be destroyed.
	h.terminals.CloseProjectTerminals(p.ID, maxTerminalGen)
	// The row is gone; reclaim the storage. A failure here is REPORTED inside
	// a successful delete: an error status would claim the project still exists.
	if h.manager != nil {
		// WithoutCancel: the row is already gone, so a client disconnect must
		// not abort the reclaim and strand the container/volume.
		reclaimCtx := context.WithoutCancel(c.Request.Context())
		spec, serr := resolveSpec(reclaimCtx, h.sandboxes, p)
		if serr == nil {
			serr = h.manager.ReclaimProject(reclaimCtx, spec)
		} else {
			h.manager.RemoveProject(p.ID)
		}
		if serr != nil {
			logging.Ctx(c.Request.Context()).Warn("reclaiming a deleted project's storage", "project", p.ID, "error", serr)
			c.JSON(http.StatusOK, projectDeleteResp{Deleted: true, StorageError: serr.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, projectDeleteResp{Deleted: true})
}
