package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// GuardrailHandler serves CRUD and catalog endpoints for guardrails.
type GuardrailHandler struct {
	store    *store.GuardrailStore
	resolver *bridge.GuardrailResolver
}

// NewGuardrailHandler returns a guardrail handler backed by the given store and resolver.
func NewGuardrailHandler(s *store.GuardrailStore, r *bridge.GuardrailResolver) *GuardrailHandler {
	return &GuardrailHandler{store: s, resolver: r}
}

// List responds with all available guardrails (stored + built-in). Stored
// entries carry config and blocking so the edit form can initialize from the
// list; built-in entries have no id and fixed behavior.
//
//	@Summary	List guardrails
//	@Tags		guardrails
//	@Produce	json
//	@Success	200	{array}	bridge.GuardrailDef
//	@Security	BearerAuth
//	@Router		/guardrails [get]
func (h *GuardrailHandler) List(c *gin.Context) {
	c.JSON(http.StatusOK, h.resolver.ListGuardrails(c.Request.Context()))
}

func validateGuardrail(g *store.Guardrail) string {
	if g.Name == "" || len(g.Stages) == 0 || g.Mode == "" {
		return "name, stages, and mode are required"
	}
	// Enforce the stage/mode enums and the mode's config (regex compiles, etc.)
	// at save time so a definition can't be stored in a state that only fails
	// when an agent references it.
	if err := bridge.ValidateGuardrailDef(g); err != nil {
		return err.Error()
	}
	return ""
}

// Create persists a new guardrail definition.
//
//	@Summary		Create guardrail
//	@Description	type: input|output; mode: regex|max_length; config: {pattern} or {max_length}.
//	@Tags			guardrails
//	@Accept			json
//	@Produce		json
//	@Param			guardrail	body		store.Guardrail	true	"Guardrail definition"
//	@Success		201			{object}	store.Guardrail
//	@Failure		400			{object}	ErrorResponse
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/guardrails [post]
func (h *GuardrailHandler) Create(c *gin.Context) {
	var g store.Guardrail
	if err := c.ShouldBindJSON(&g); err != nil {
		badRequest(c, err.Error())
		return
	}
	if msg := validateGuardrail(&g); msg != "" {
		badRequest(c, msg)
		return
	}
	if err := h.store.Create(c.Request.Context(), &g); err != nil {
		saveError(c, err) // duplicate (type, name) -> 409
		return
	}
	created(c, g.ID, g)
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
func (h *GuardrailHandler) Get(c *gin.Context) {
	g, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, g)
}

// Update overwrites a guardrail definition and responds with the updated
// guardrail.
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
func (h *GuardrailHandler) Update(c *gin.Context) {
	var g store.Guardrail
	if err := c.ShouldBindJSON(&g); err != nil {
		badRequest(c, err.Error())
		return
	}
	if msg := validateGuardrail(&g); msg != "" {
		badRequest(c, msg)
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	if err := h.store.Update(ctx, id, &g); err != nil {
		saveError(c, err) // duplicate (type, name) -> 409, not-found -> 404
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, updated)
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
func (h *GuardrailHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
