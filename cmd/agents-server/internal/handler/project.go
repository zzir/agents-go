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
	sandboxes *store.SandboxStore
	manager   *sandboxes.Manager
	terminals *TerminalHandler
}

// NewProjectHandler returns a handler over the project store; sandboxes
// validates the target a new project names, m reclaims a deleted project's
// cached instance and runs the container calls, and terminals is the registry
// an environment change severs.
func NewProjectHandler(s *store.ProjectStore, sb *store.SandboxStore, m *sandboxes.Manager, terminals *TerminalHandler) *ProjectHandler {
	return &ProjectHandler{store: s, sandboxes: sb, manager: m, terminals: terminals}
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

// storageHint derives where p's files live — the workspace directory on the
// local daemon, the named volume on a remote one — so the UI can say what a
// delete leaves behind (decisions §5.28: storage outlives the row). Admin-only: a
// host path is a server-side fact a member's container never sees. hosts
// caches sandbox→host across one response; empty when it cannot be derived.
func (h *ProjectHandler) storageHint(c *gin.Context, hosts map[string]string, p *store.Project) string {
	if h.manager == nil {
		return ""
	}
	host, ok := hosts[p.SandboxID]
	if !ok {
		cfg, err := h.sandboxes.Get(c.Request.Context(), p.SandboxID)
		if err != nil {
			return ""
		}
		var dc store.DockerConfig
		_ = json.Unmarshal(cfg.Config, &dc)
		host = dc.Host
		hosts[p.SandboxID] = host
	}
	if host == "" {
		return h.manager.ProjectHostDir(p)
	}
	return "docker volume " + sandboxes.ProjectVolumeName(p.ID) + " on " + host
}

type projectReq struct {
	Name      string `json:"name" binding:"required"`
	SandboxID string `json:"sandbox_id" binding:"required"`
	// Env is the environment the project's container is created with;
	// optional, and empty means none.
	Env []store.EnvVar `json:"env,omitempty"`
}

// projectUpdateReq is the update body: the name, the whole environment, and
// the revision the edit was made against (optional — see Update).
type projectUpdateReq struct {
	Name     string         `json:"name" binding:"required"`
	Env      []store.EnvVar `json:"env,omitempty"`
	Revision int64          `json:"revision,omitempty"`
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
	// The sandbox's existence is the create's own guard (the store locks the
	// row), so a missing target 404s from the insert itself — no pre-check.
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
		badRequest(c, "name is required")
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
	contentChanged := !store.EnvContentEqual(prev.Env, env)
	next := *prev
	next.Name, next.Env = req.Name, env
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

// PrepareContainer creates the project's container now.
//
//	@Summary		Prepare container
//	@Description	Creates the container up front — the first run otherwise waits for it (an image pull included) inside its first tool call. Synchronous.
//	@Tags			projects
//	@Param			id	path	string	true	"Project id"
//	@Success		204	"ready"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id}/container/prepare [post]
func (h *ProjectHandler) PrepareContainer(c *gin.Context) {
	h.containerAct(c, h.manager.PrepareContainer)
}

// RebuildContainer discards the project's container and creates a fresh one.
//
//	@Summary		Rebuild container
//	@Description	Discards the container and creates a fresh one from the current image and environment. Files under /workspace survive; anything installed into the container does not, and commands running in it fail. Synchronous.
//	@Tags			projects
//	@Param			id	path	string	true	"Project id"
//	@Success		204	"rebuilt"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id}/container/rebuild [post]
func (h *ProjectHandler) RebuildContainer(c *gin.Context) {
	h.containerAct(c, h.manager.RebuildContainer)
}

// containerAct resolves the caller's project and its sandbox, then runs one
// of the manager's container calls against the pair.
func (h *ProjectHandler) containerAct(c *gin.Context, act func(context.Context, *store.SandboxConfig, *store.Project) error) {
	p, ok := h.own(c)
	if !ok {
		return
	}
	cfg, err := h.sandboxes.Get(c.Request.Context(), p.SandboxID)
	if err != nil {
		storeError(c, err)
		return
	}
	if err := act(c.Request.Context(), cfg, p); err != nil {
		upstreamError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Delete removes the caller's project — an admin's: any project — while no
// session binds it. The project's files (host directory or remote volume)
// are left in place — data outlives the row on purpose.
//
//	@Summary		Delete project
//	@Description	The owner deletes their own; an admin deletes any (management, decisions §5.29).
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
	if h.manager != nil {
		h.manager.RemoveProject(p.ID) // reclaim the cached instance; storage stays
	}
	c.Status(http.StatusNoContent)
}
