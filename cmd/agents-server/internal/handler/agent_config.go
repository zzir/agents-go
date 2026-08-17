// Package handler implements the HTTP and WebSocket request handlers for the agents-server API.
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// AgentConfigHandler serves CRUD endpoints for agent configurations.
type AgentConfigHandler struct {
	store *store.AgentConfigStore
	// mcpServers, when set, lets save-time validation resolve the MCP server
	// ids referenced by the tools field and predict tool-name collisions.
	mcpServers *store.McpServerStore
	// guardrails, when set, lets save-time validation reject unresolvable
	// guardrail names before they silently no-op at run time.
	guardrails *bridge.GuardrailResolver
	// providers, when set, lets save-time validation reject a provider_id that
	// names no row — the write side of referential integrity, whose read side
	// is ProviderStore.DeleteIfUnreferenced.
	providers *store.ProviderStore
}

// NewAgentConfigHandler returns a handler over the agent store and the three
// stores its validation reads (the MCP servers, providers and guardrails a
// config may name). Every one is required; a nil is a wiring error.
func NewAgentConfigHandler(s *store.AgentConfigStore, mcpServers *store.McpServerStore, providers *store.ProviderStore, guardrails *bridge.GuardrailResolver) *AgentConfigHandler {
	if s == nil || mcpServers == nil || providers == nil || guardrails == nil {
		panic("handler: NewAgentConfigHandler needs every store")
	}
	return &AgentConfigHandler{store: s, mcpServers: mcpServers, providers: providers, guardrails: guardrails}
}

// validateAgentConfig checks an incoming Create/Update body against the
// constraints the run would otherwise only hit at run time. It reports the
// failure to c and returns false when the request is rejected. Name uniqueness
// is enforced by the DB (mapped to 409 by saveError), not checked here.
func (h *AgentConfigHandler) validateAgentConfig(c *gin.Context, ac *store.AgentConfig) bool {
	if ac.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	// The OpenAI provider ships no built-in default model, so an empty model no
	// longer silently falls back — it becomes a *UserError at run time. Reject it
	// at save time with a clear message instead.
	if ac.Model == "" {
		badRequest(c, "model is required")
		return false
	}
	// An agent naming a provider that does not exist would fail at run time
	// with a confusing "provider not found" mid-stream; refuse at save.
	if ac.ProviderID != "" {
		if _, err := h.providers.Get(c.Request.Context(), ac.ProviderID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				badRequest(c, "provider_id names no provider")
			} else {
				internalError(c, err)
			}
			return false
		}
	}
	if err := bridge.ValidateAgentToolNames(c.Request.Context(), h.mcpServers, ac.ToolsJSON); err != nil {
		badRequest(c, err.Error())
		return false
	}
	// Reject config whose JSON-encoded fields don't parse/resolve, rather than
	// silently no-op'ing at run time (a guardrail or output schema that "looks
	// enabled" but never runs is the dangerous case). The same decode backs the
	// build, so the structural contract lives in exactly one place.
	if _, err := bridge.DecodeAgentSpec(ac); err != nil {
		badRequest(c, err.Error())
		return false
	}
	if err := h.guardrails.ValidateNames(c.Request.Context(), ac.Guardrails.Guardrails); err != nil {
		badRequest(c, err.Error())
		return false
	}
	return true
}

// List responds with all agent configurations, secrets masked.
//
//	@Summary	List agents
//	@Tags		agents
//	@Produce	json
//	@Success	200	{array}		store.AgentConfig
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/agents [get]
func (h *AgentConfigHandler) List(c *gin.Context) {
	configs, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range configs {
		sanitizeAgentConfig(&configs[i])
	}
	c.JSON(http.StatusOK, configs)
}

// Create persists a new agent configuration from the request body.
//
//	@Summary		Create agent
//	@Description	Secret fields (api_key, fallback_models[].api_key) are write-only: responses mask them with ********; sending the mask back keeps the stored value, "" clears it. Tool selections whose statically known tool names would collide are rejected.
//	@Tags			agents
//	@Accept			json
//	@Produce		json
//	@Param			agent	body		store.AgentConfig	true	"Agent configuration"
//	@Success		201		{object}	store.AgentConfig
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents [post]
func (h *AgentConfigHandler) Create(c *gin.Context) {
	var ac store.AgentConfig
	if err := c.ShouldBindJSON(&ac); err != nil {
		badRequest(c, err.Error())
		return
	}
	// id/timestamps are server-owned (BeforeAppendModel stamps them).
	ac.ID = ""
	if !h.validateAgentConfig(c, &ac) {
		return
	}
	// There is no stored value yet, so a mask sentinel resolves to empty.
	ac.Resilience.FallbackModels = restoreFallbackModels(ac.Resilience.FallbackModels, "")
	if err := h.store.Create(c.Request.Context(), &ac); err != nil {
		saveError(c, err) // duplicate name -> 409
		return
	}
	sanitizeAgentConfig(&ac)
	c.JSON(http.StatusCreated, ac)
}

// Get responds with the agent configuration identified by the id path
// parameter, secrets masked.
//
//	@Summary	Get agent
//	@Tags		agents
//	@Produce	json
//	@Param		id	path		string	true	"Agent ID"
//	@Success	200	{object}	store.AgentConfig
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/agents/{id} [get]
func (h *AgentConfigHandler) Get(c *gin.Context) {
	ac, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	sanitizeAgentConfig(ac)
	c.JSON(http.StatusOK, ac)
}

// Update overwrites the agent configuration identified by the id path
// parameter and responds with the updated configuration (secrets masked).
// Masked secret fields keep their stored values.
//
//	@Summary		Update agent
//	@Description	Full replace. Secret fields are write-only: send back the ******** mask to keep the stored value, "" to clear it; a mask kept across a provider_type or base_url change is rejected (the stored key belongs to the previous destination). Tool selections whose statically known tool names would collide are rejected.
//	@Tags			agents
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string				true	"Agent ID"
//	@Param			agent	body		store.AgentConfig	true	"Agent configuration"
//	@Success		200		{object}	store.AgentConfig
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents/{id} [put]
func (h *AgentConfigHandler) Update(c *gin.Context) {
	var ac store.AgentConfig
	if err := c.ShouldBindJSON(&ac); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validateAgentConfig(c, &ac) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	// Load the current row so the masked fallback-model keys can round-trip to
	// their stored values. A transient (non-not-found) Get failure must abort:
	// continuing with an empty prev would resolve the ******** mask to "" and
	// silently WIPE them. Not-found is fine to carry through — the Update below
	// returns 404 for it.
	prev, err := h.store.Get(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(c, err)
		return
	}
	var prevFallback string
	if prev != nil {
		prevFallback = prev.Resilience.FallbackModels
	}
	ac.Resilience.FallbackModels = restoreFallbackModels(ac.Resilience.FallbackModels, prevFallback)
	if err := h.store.Update(ctx, id, &ac); err != nil {
		saveError(c, err) // duplicate name -> 409, not-found -> 404
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	sanitizeAgentConfig(updated)
	c.JSON(http.StatusOK, updated)
}

// Delete removes the agent configuration identified by the id path parameter.
//
//	@Summary	Delete agent
//	@Tags		agents
//	@Param		id	path	string	true	"Agent ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/agents/{id} [delete]
func (h *AgentConfigHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
