package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProjectHandler manages projects — per-user working trees on a sandbox
// (decisions §5.28). Projects are PERSONAL: routes act on the caller's own rows;
// an admin additionally lists every owner's (?all=true) and may delete any —
// management, not authoring (decisions §5.29).
type ProjectHandler struct {
	store     *store.ProjectStore
	targets   *store.SandboxTargetStore
	templates *store.SandboxTemplateStore
	manager   *sandboxes.Manager
	terminals *TerminalHandler
}

// NewProjectHandler returns a handler over the project store; targets and
// templates validate what a project names, m reclaims a deleted project's
// storage and runs the container calls, and terminals is the registry a
// content change severs.
func NewProjectHandler(s *store.ProjectStore, targets *store.SandboxTargetStore, templates *store.SandboxTemplateStore, m *sandboxes.Manager, terminals *TerminalHandler) *ProjectHandler {
	return &ProjectHandler{store: s, targets: targets, templates: templates, manager: m, terminals: terminals}
}

// projectDetail is the single-project response: the row plus the NAMES of
// its environment, every value masked. Env shadows the row's own (json:"-")
// field on purpose — a listing must never carry one.
type projectDetail struct {
	store.Project
	Env []store.EnvVar `json:"env"`
}

// maskProjectEnv replaces every value with the sentinel a later update
// resolves back (restoreProjectEnv). Values are write-only, like every other
// credential here; names stay readable, so the environment can still be
// edited a variable at a time (decisions §5.32).
func maskProjectEnv(vars []store.EnvVar) []store.EnvVar {
	out := make([]store.EnvVar, 0, len(vars))
	for _, v := range vars {
		v.Value = maskSecret(v.Value) // an empty value stays empty, as everywhere else
		out = append(out, v)
	}
	return out
}

// restoreProjectEnv resolves masked values against the stored ones BY NAME —
// so an edit rewrites the one variable it touches and leaves the rest
// standing, and a mask can never ride to a name it was not stored under
// (workbench invariant 9).
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
// undecodable stored payload is reported rather than silently shown empty:
// the owner has to be able to see that something is wrong with it.
func (h *ProjectHandler) detail(p *store.Project) (*projectDetail, error) {
	vars, err := store.DecodeProjectEnv(p.Env)
	if err != nil {
		return nil, err
	}
	return &projectDetail{Project: *p, Env: maskProjectEnv(vars)}, nil
}

// own resolves the caller's project by id; an admin's management reach does
// NOT extend here — an environment is the owner's, and a foreign project
// reads as absent.
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
	if admin {
		hosts := map[string]string{}
		for i := range out {
			out[i].StorageHint = h.storageHint(c, hosts, &out[i])
		}
	}
	c.JSON(http.StatusOK, out)
}

// storageHint names the volume p's files live in and the daemon it is on, so
// the UI can say what a delete destroys (decisions §5.33). Admin-only: a
// daemon address is a server-side fact a member's container never sees. hosts
// caches target→host across one response; empty when it cannot be derived.
func (h *ProjectHandler) storageHint(c *gin.Context, hosts map[string]string, p *store.Project) string {
	host, ok := hosts[p.TargetID]
	if !ok {
		t, err := h.targets.Get(c.Request.Context(), p.TargetID)
		if err != nil {
			return ""
		}
		var dc store.DockerTargetConfig
		_ = json.Unmarshal(t.Config, &dc)
		host = dc.Host
		if host == "" {
			host = "the local daemon"
		}
		hosts[p.TargetID] = host
	}
	return "docker volume " + sandboxes.ProjectVolumeName(p.ID) + " on " + host
}

type projectReq struct {
	Name string `json:"name" binding:"required"`
	// TargetID is the machine the tree lives on — frozen after creation.
	TargetID string `json:"target_id" binding:"required"`
	// TemplateID is what the container is created from — editable.
	TemplateID string `json:"template_id" binding:"required"`
	// Env is the environment the project's container is created with;
	// optional, and empty means none.
	Env []store.EnvVar `json:"env,omitempty"`
}

// projectUpdateReq is the update body: the name, the template, the whole
// environment, and the revision the edit was made against (optional — see
// Update). The target is not here: it is the project's identity.
type projectUpdateReq struct {
	Name       string         `json:"name" binding:"required"`
	TemplateID string         `json:"template_id" binding:"required"`
	Env        []store.EnvVar `json:"env,omitempty"`
	Revision   int64          `json:"revision,omitempty"`
}

// Create makes a new project for the caller on the named target, from the
// named template.
//
//	@Summary	Create project
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		project	body		projectReq	true	"Name, target and template"
//	@Success	201		{object}	projectDetail
//	@Failure	400		{object}	ErrorResponse
//	@Failure	409		{object}	ErrorResponse	"name already in use on this target"
//	@Security	BearerAuth
//	@Router		/projects [post]
func (h *ProjectHandler) Create(c *gin.Context) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	var req projectReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "name, target_id and template_id are required")
		return
	}
	env, err := store.NormalizeProjectEnv(req.Env)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.typesMatch(c, req.TargetID, req.TemplateID) {
		return
	}
	// Existence is the create's own guard (the store locks both rows), so a
	// missing target or template 404s from the insert itself — the type check
	// above is the one thing the insert cannot express.
	p := &store.Project{OwnerID: ownerID, TargetID: req.TargetID, TemplateID: req.TemplateID, Name: req.Name, Env: env}
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
//	@Description	A value sent back as its mask keeps what is stored; any other value replaces it. An environment change replaces the project's container at its next run and severs its terminals; a rename does neither.
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
		badRequest(c, "name and template_id are required")
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
	// Everything decided from prev holds only while the row IS prev, which
	// the expected-revision CAS guarantees: a concurrent update moves the
	// revision, this write refuses (409), the client re-reads. The anchor is
	// the client's own revision when it names one, extending the guarantee
	// back to the form the edit was made on.
	expected := prev.Revision
	if req.Revision != 0 {
		expected = req.Revision
	}
	if req.TemplateID != prev.TemplateID && !h.typesMatch(c, prev.TargetID, req.TemplateID) {
		return
	}
	contentChanged := !store.EnvContentEqual(prev.Env, env) || req.TemplateID != prev.TemplateID
	next := *prev
	next.Name, next.TemplateID, next.Env = req.Name, req.TemplateID, env
	if err := h.store.Update(c.Request.Context(), prev.ID, &next, expected, contentChanged); err != nil {
		saveError(c, err)
		return
	}
	// Invalidate from what the CAS guarantees (the generation moved iff the
	// content changed), not from a re-read a cancelled request could fail —
	// which would leave the new environment stored while live containers and
	// terminals keep serving the old one.
	if contentChanged {
		h.manager.RetireProject(prev.ID, prev.RuntimeGen+1)
		h.terminals.CloseProjectTerminals(prev.ID, prev.RuntimeGen+1)
	}
	// Re-read for the response: the counters the write moved live in the
	// row, and a client that answered with a stale revision would have its
	// next update refused as a conflict.
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
//	@Description	Discards the container and creates a fresh one from the current template and environment. Files under /workspace survive; anything installed into the container does not, and commands running in it fail. Synchronous.
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
//	@Description	Synchronous, and can take an image pull's worth of time.
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
//	@Description	Keeps the working tree. `stopped: false` means a run or terminal is still using it and the stop happens when that ends.
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

// spec resolves the caller's project into a build spec, answering the error
// when it cannot.
func (h *ProjectHandler) spec(c *gin.Context) (sandboxes.Spec, bool) {
	p, ok := h.own(c)
	if !ok {
		return sandboxes.Spec{}, false
	}
	spec, err := resolveSpec(c.Request.Context(), h.targets, h.templates, p)
	if err != nil {
		storeError(c, err)
		return sandboxes.Spec{}, false
	}
	return spec, true
}

// containerAct resolves the caller's project into a build spec, then runs one
// of the manager's sandbox calls against it.
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

// typesMatch refuses a project whose template cannot run on its target — the
// one cross-row rule neither insert nor update can express in SQL.
func (h *ProjectHandler) typesMatch(c *gin.Context, targetID, templateID string) bool {
	ctx := c.Request.Context()
	t, err := h.targets.Get(ctx, targetID)
	if err != nil {
		storeError(c, err)
		return false
	}
	tpl, err := h.templates.Get(ctx, templateID)
	if err != nil {
		storeError(c, err)
		return false
	}
	if t.Type != tpl.Type {
		badRequest(c, fmt.Sprintf("template %q is a %s template and target %q is a %s target", tpl.Name, tpl.Type, t.Name, t.Type))
		return false
	}
	return true
}

// Delete removes the caller's project — an admin's: any project — while no
// session binds it, then DESTROYS its storage: the container and the volume
// holding the working tree (decisions §5.33).
//
//	@Summary		Delete project
//	@Description	Deletes the working tree too — the container and its volume are removed. The owner deletes their own; an admin deletes any (management, decisions §5.29).
//	@Tags			projects
//	@Param			id	path	string	true	"Project id"
//	@Success		204	"deleted"
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
	// The row is gone; reclaim the storage. A failure here leaves reclaimable
	// storage rather than a row pointing at nothing, so it is reported without
	// undoing the delete.
	if h.manager != nil {
		spec, serr := resolveSpec(c.Request.Context(), h.targets, h.templates, p)
		if serr != nil {
			h.manager.RemoveProject(p.ID)
			internalError(c, serr)
			return
		}
		if rerr := h.manager.ReclaimProject(c.Request.Context(), spec); rerr != nil {
			upstreamError(c, rerr)
			return
		}
	}
	c.Status(http.StatusNoContent)
}
