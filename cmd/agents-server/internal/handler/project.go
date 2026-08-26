package handler

import (
	"encoding/json"
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
}

// NewProjectHandler returns a handler over the project store; sandboxes
// validates the target a new project names, and m reclaims a deleted
// project's cached instance.
func NewProjectHandler(s *store.ProjectStore, sb *store.SandboxStore, m *sandboxes.Manager) *ProjectHandler {
	return &ProjectHandler{store: s, sandboxes: sb, manager: m}
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
}

// Create makes a new project for the caller on the named sandbox.
//
//	@Summary	Create project
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		project	body		projectReq	true	"Name and sandbox"
//	@Success	201		{object}	store.Project
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
	// The sandbox's existence is the create's own guard (the store locks the
	// row), so a missing target 404s from the insert itself — no pre-check.
	p := &store.Project{OwnerID: ownerID, SandboxID: req.SandboxID, Name: req.Name}
	if err := h.store.Create(c.Request.Context(), p); err != nil {
		saveError(c, err)
		return
	}
	if admin {
		p.StorageHint = h.storageHint(c, map[string]string{}, p)
	}
	created(c, p.ID, p)
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
