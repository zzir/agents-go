package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// GuardrailHandler serves CRUD and catalog endpoints for guardrails.
type GuardrailHandler struct {
	store    *store.GuardrailStore
	resolver *guardrails.Resolver
}

// NewGuardrailHandler returns a guardrail handler backed by the given store and
// resolver.
func NewGuardrailHandler(s *store.GuardrailStore, r *guardrails.Resolver) *GuardrailHandler {
	return &GuardrailHandler{store: s, resolver: r}
}

// List responds with every guardrail: stored (config and blocking included —
// invariant 2) and built-in (no id, fixed behavior).
//
//	@Summary	List guardrails
//	@Tags		guardrails
//	@Produce	json
//	@Success	200	{array}	guardrails.Def
//	@Security	BearerAuth
//	@Router		/guardrails [get]
func (h *GuardrailHandler) List(c *gin.Context) error {
	c.JSON(http.StatusOK, nonNilList(h.resolver.List(c.Request.Context())))
	return nil
}

func validateGuardrail(g *store.Guardrail) error {
	if g.Name == "" || len(g.Stages) == 0 || g.Mode == "" {
		return badRequest("name, stages, and mode are required")
	}
	// Validate the stages, the mode and its config at save time.
	if err := guardrails.ValidateDef(g); err != nil {
		return badRequest(err.Error())
	}
	return nil
}

// Create persists a new guardrail definition.
//
//	@Summary		Create guardrail
//	@Description	stages: any of input|output|tool_input|tool_output; mode: regex|max_length; config: {pattern} or {max_length}.
//	@Tags			guardrails
//	@Accept			json
//	@Produce		json
//	@Param			guardrail	body		store.Guardrail	true	"Guardrail definition"
//	@Success		201			{object}	store.Guardrail
//	@Failure		400			{object}	ErrorResponse
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/guardrails [post]
func (h *GuardrailHandler) Create(c *gin.Context) error {
	var g store.Guardrail
	if err := c.ShouldBindJSON(&g); err != nil {
		return badRequest(err.Error())
	}
	if err := validateGuardrail(&g); err != nil {
		return err
	}
	g.ID, g.CreatedAt = "", time.Time{} // server-owned
	if err := h.store.Create(c.Request.Context(), &g); err != nil {
		return err // duplicate (type, name) -> 409
	}
	created(c, g.ID, g)
	return nil
}

// Get responds with a single guardrail by ID.
//
//	@Summary	Get guardrail
//	@Tags		guardrails
//	@Produce	json
//	@Param		id	path		string	true	"Guardrail ID"
//	@Success	200	{object}	store.Guardrail
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/guardrails/{id} [get]
func (h *GuardrailHandler) Get(c *gin.Context) error {
	g, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, g)
	return nil
}

// Update overwrites a guardrail definition and responds with it.
//
//	@Summary	Update guardrail
//	@Tags		guardrails
//	@Accept		json
//	@Produce	json
//	@Param		id			path		string			true	"Guardrail ID"
//	@Param		guardrail	body		store.Guardrail	true	"Guardrail definition"
//	@Success	200			{object}	store.Guardrail
//	@Failure	400			{object}	ErrorResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/guardrails/{id} [put]
func (h *GuardrailHandler) Update(c *gin.Context) error {
	var g store.Guardrail
	if err := c.ShouldBindJSON(&g); err != nil {
		return badRequest(err.Error())
	}
	if err := validateGuardrail(&g); err != nil {
		return err
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	if err := h.store.Update(ctx, id, &g); err != nil {
		return err // duplicate (type, name) -> 409, not-found -> 404
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, updated)
	return nil
}

// Delete removes a guardrail by ID.
//
//	@Summary	Delete guardrail
//	@Tags		guardrails
//	@Param		id	path	string	true	"Guardrail ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/guardrails/{id} [delete]
func (h *GuardrailHandler) Delete(c *gin.Context) error {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}
