package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// SandboxTemplateHandler serves CRUD endpoints for sandbox templates — what a
// project's container is created from (decisions §5.33).
type SandboxTemplateHandler struct {
	store  *store.SandboxTemplateStore
	retire *Retirer
}

// NewSandboxTemplateHandler returns a handler over the template store; the
// retirer is what carries a content change to the projects using it.
func NewSandboxTemplateHandler(s *store.SandboxTemplateStore, r *Retirer) *SandboxTemplateHandler {
	if s == nil || r == nil {
		panic("handler: NewSandboxTemplateHandler needs the template store and a retirer")
	}
	return &SandboxTemplateHandler{store: s, retire: r}
}

// List responds with all sandbox templates.
//
//	@Summary	List sandbox templates
//	@Tags		sandbox-templates
//	@Produce	json
//	@Success	200	{array}		store.SandboxTemplate
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-templates [get]
func (h *SandboxTemplateHandler) List(c *gin.Context) {
	out, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

type sandboxTemplateReq struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
	// Revision, on update only, is the revision the client's edit was based
	// on — see sandboxTargetReq.Revision.
	Revision int64 `json:"revision,omitempty"`
}

// validate enforces name and type; field-level validation lives in
// store.NormalizeTemplateConfig, which both write handlers run right after.
func (h *SandboxTemplateHandler) validate(c *gin.Context, req *sandboxTemplateReq) bool {
	if req.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	if req.Type == "" {
		badRequest(c, "type is required")
		return false
	}
	if !slices.Contains(store.TargetTypes, req.Type) {
		badRequest(c, "type must be one of "+strings.Join(store.TargetTypes, ", ")+", got "+req.Type)
		return false
	}
	return true
}

// Create persists a new sandbox template.
//
//	@Summary		Create sandbox template
//	@Description	type "docker" config: image (required), runtime, user ("" = root), network (docker network name; "" = no network), memory_mb/cpus caps, max_read_file_bytes. type "e2b" config: template_id (required — build it on the service first), timeout_seconds, auto_pause, allow_internet, max_read_file_bytes.
//	@Tags			sandbox-templates
//	@Accept			json
//	@Produce		json
//	@Param			template	body		sandboxTemplateReq	true	"Sandbox template"
//	@Success		201			{object}	store.SandboxTemplate
//	@Failure		400			{object}	ErrorResponse
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandbox-templates [post]
func (h *SandboxTemplateHandler) Create(c *gin.Context) {
	var req sandboxTemplateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validate(c, &req) {
		return
	}
	canonical, err := store.NormalizeTemplateConfig(req.Type, req.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	t := &store.SandboxTemplate{Name: req.Name, Type: req.Type, Config: canonical}
	if err := h.store.Create(c.Request.Context(), t); err != nil {
		internalError(c, err)
		return
	}
	created(c, t.ID, t)
}

// Get responds with one sandbox template.
//
//	@Summary	Get sandbox template
//	@Tags		sandbox-templates
//	@Produce	json
//	@Param		id	path		string	true	"Template id"
//	@Success	200	{object}	store.SandboxTemplate
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-templates/{id} [get]
func (h *SandboxTemplateHandler) Get(c *gin.Context) {
	t, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// Update overwrites the template. A content change replaces the container of
// every project using it at that project's next run, and severs its terminals.
// The type is immutable (store.ErrTemplateTypeImmutable).
//
//	@Summary		Update sandbox template
//	@Description	Include the revision the edit was based on to make the write conditional: 409 if the row changed meanwhile. The type cannot change — create a new template instead.
//	@Tags			sandbox-templates
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string				true	"Template id"
//	@Param			template	body		sandboxTemplateReq	true	"Sandbox template"
//	@Success		200			{object}	store.SandboxTemplate
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"the row changed concurrently — re-read and retry"
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandbox-templates/{id} [put]
func (h *SandboxTemplateHandler) Update(c *gin.Context) {
	var req sandboxTemplateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validate(c, &req) {
		return
	}
	id := c.Param("id")
	ctx := c.Request.Context()
	prev, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if prev.Type != req.Type {
		badRequest(c, store.ErrTemplateTypeImmutable.Error())
		return
	}
	canonical, err := store.NormalizeTemplateConfig(req.Type, req.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	expected := prev.Revision
	if req.Revision != 0 {
		expected = req.Revision
	}
	next := *prev
	next.Name, next.Config = req.Name, canonical
	contentChanged := !store.TemplateContentEqual(prev.Type, prev.Config, canonical)
	if err := h.store.Update(ctx, id, &next, expected); err != nil {
		storeError(c, err)
		return
	}
	// Invalidate from what the CAS guarantees, not from a re-read a cancelled
	// request could fail — which would leave the new image stored while live
	// containers and terminals keep serving the old one.
	if contentChanged {
		if err := h.retire.bump(ctx, "template_id", id); err != nil {
			internalError(c, err)
			return
		}
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete removes the sandbox template. One still used by projects is refused
// with 409 — the refusal is decided by the delete statement itself, so a
// project create racing it either lands before it (and blocks it) or loses.
//
//	@Summary	Delete sandbox template
//	@Tags		sandbox-templates
//	@Param		id	path	string	true	"Template id"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"projects use this template"
//	@Security	BearerAuth
//	@Router		/sandbox-templates/{id} [delete]
func (h *SandboxTemplateHandler) Delete(c *gin.Context) {
	projects, err := h.store.DeleteIfUnreferenced(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if projects > 0 {
		conflict(c, fmt.Sprintf("%d project(s) use this template; move or delete them first", projects))
		return
	}
	c.Status(http.StatusNoContent)
}
