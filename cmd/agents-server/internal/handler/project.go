package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProjectHandler manages projects — per-user working trees on a sandbox
// (spec §5.28). Projects are PERSONAL: every route acts on the caller's own
// rows, so none of these carry the admin gate shared configuration does.
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

// List responds with the caller's projects.
//
//	@Summary	List my projects
//	@Tags		projects
//	@Produce	json
//	@Success	200	{array}	store.Project
//	@Security	BearerAuth
//	@Router		/projects [get]
func (h *ProjectHandler) List(c *gin.Context) {
	u, ok := server.CurrentUser(c)
	if !ok {
		notFound(c)
		return
	}
	out, err := h.store.ListByOwner(c.Request.Context(), u.ID)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
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
	u, ok := server.CurrentUser(c)
	if !ok {
		notFound(c)
		return
	}
	var req projectReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "name and sandbox_id are required")
		return
	}
	// The sandbox's existence is the create's own guard (the store locks the
	// row), so a missing target 404s from the insert itself — no pre-check.
	p := &store.Project{OwnerID: u.ID, SandboxID: req.SandboxID, Name: req.Name}
	if err := h.store.Create(c.Request.Context(), p); err != nil {
		saveError(c, err)
		return
	}
	created(c, p.ID, p)
}

// Delete removes the caller's project while no session binds it. The
// project's files (host directory or remote volume) are left in place — data
// outlives the row on purpose.
//
//	@Summary	Delete project
//	@Tags		projects
//	@Param		id	path	string	true	"Project id"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"sessions still bound"
//	@Security	BearerAuth
//	@Router		/projects/{id} [delete]
func (h *ProjectHandler) Delete(c *gin.Context) {
	u, ok := server.CurrentUser(c)
	if !ok {
		notFound(c)
		return
	}
	p, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if p.OwnerID != u.ID {
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
