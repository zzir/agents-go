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
}

// NewAgentConfigHandler returns a handler backed by the given store.
func NewAgentConfigHandler(s *store.AgentConfigStore) *AgentConfigHandler {
	return &AgentConfigHandler{store: s}
}

// WithMcpStore attaches the MCP server store used to validate the MCP servers
// referenced by an agent's tools field. It returns h for chaining.
func (h *AgentConfigHandler) WithMcpStore(m *store.McpServerStore) *AgentConfigHandler {
	h.mcpServers = m
	return h
}

// WithGuardrails attaches the guardrail resolver used to validate the guardrail
// names referenced by an agent config. It returns h for chaining.
func (h *AgentConfigHandler) WithGuardrails(g *bridge.GuardrailResolver) *AgentConfigHandler {
	h.guardrails = g
	return h
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
	if ac.Session.UsePreviousResponseID {
		badRequest(c, "use_previous_response_id is not supported: agents-server always persists conversation history in a server-side session, which cannot be combined with previous-response chaining — disable use_previous_response_id")
		return false
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
	if h.guardrails != nil {
		if err := h.guardrails.ValidateNames(c.Request.Context(), ac.Guardrails.InputGuardrails, ac.Guardrails.OutputGuardrails); err != nil {
			badRequest(c, err.Error())
			return false
		}
	}
	return true
}

// List responds with all agent configurations, secrets masked.
//
//	@Summary	List agents
//	@Tags agents
//	@Produce	json
//	@Success	200	{array} store.AgentConfig
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router /agents [get]
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
//	@Summary Create agent
//	@Description	Secret fields (api_key, fallback_models[].api_key) are write-only: responses mask them with ********; sending the mask back keeps the stored value, "" clears it. use_previous_response_id is rejected (incompatible with the server-side session store), as are tool selections whose statically known tool names would collide.
//	@Tags agents
//	@Accept json
//	@Produce json
//	@Param agent	body store.AgentConfig	true	"Agent configuration"
//	@Success 201 {object}	store.AgentConfig
//	@Failure 400 {object}	ErrorResponse
//	@Failure 500 {object}	ErrorResponse
//	@Security BearerAuth
//	@Router /agents [post]
func (h *AgentConfigHandler) Create(c *gin.Context) {
	var ac store.AgentConfig
	if err := c.ShouldBindJSON(&ac); err != nil {
		badRequest(c, err.Error())
		return
	}
	// id/timestamps are server-owned (BeforeAppendModel stamps them); the ChatGPT
	// token is set only via the OAuth flow, never a CRUD body.
	ac.ID, ac.ChatGPTToken = "", ""
	if !h.validateAgentConfig(c, &ac) {
		return
	}
	// There is no stored value yet, so a mask sentinel resolves to empty.
	ac.Provider.APIKey = resolveSecret(ac.Provider.APIKey, "")
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
//	@Tags agents
//	@Produce	json
//	@Param id	path string	true	"Agent ID"
//	@Success	200	{object}	store.AgentConfig
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router /agents/{id} [get]
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
//	@Summary Update agent
//	@Description	Full replace. Secret fields are write-only: send back the ******** mask to keep the stored value, "" to clear it. use_previous_response_id is rejected (incompatible with the server-side session store), as are tool selections whose statically known tool names would collide.
//	@Tags agents
//	@Accept json
//	@Produce json
//	@Param id path string true	"Agent ID"
//	@Param agent	body store.AgentConfig	true	"Agent configuration"
//	@Success 200 {object}	store.AgentConfig
//	@Failure 400 {object}	ErrorResponse
//	@Failure 404 {object}	ErrorResponse
//	@Failure 500 {object}	ErrorResponse
//	@Security BearerAuth
//	@Router /agents/{id} [put]
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
	// Load the current row so masked secrets can round-trip to their stored
	// values. A transient (non-not-found) Get failure must abort: continuing with
	// an empty prev would resolve the ******** mask to "" and silently WIPE the
	// stored api_key. Not-found is fine to carry through — the Update below
	// returns 404 for it.
	prev, err := h.store.Get(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(c, err)
		return
	}
	var prevKey, prevFallback string
	if prev != nil {
		prevKey, prevFallback = prev.Provider.APIKey, prev.Resilience.FallbackModels
	}
	ac.Provider.APIKey = resolveSecret(ac.Provider.APIKey, prevKey)
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
//	@Tags agents
//	@Param id	path	string	true	"Agent ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router /agents/{id} [delete]
func (h *AgentConfigHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
