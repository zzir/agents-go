// Package handler implements the HTTP and WebSocket request handlers for the agents-server API.
package handler

import (
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
func (h *AgentConfigHandler) validateAgentConfig(c *gin.Context, req *agentConfigReq) bool {
	if req.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	if req.UsePreviousResponseID {
		badRequest(c, "use_previous_response_id is not supported: agents-server always persists conversation history in a server-side session, which cannot be combined with previous-response chaining — disable use_previous_response_id")
		return false
	}
	if err := bridge.ValidateAgentToolNames(c.Request.Context(), h.mcpServers, req.ToolsJSON); err != nil {
		badRequest(c, err.Error())
		return false
	}
	// Reject config whose JSON-encoded fields don't parse/resolve, rather than
	// silently no-op'ing at run time (a guardrail or output schema that "looks
	// enabled" but never runs is the dangerous case). The same decode backs the
	// build, so the structural contract lives in exactly one place.
	if _, err := bridge.DecodeAgentSpec(req.toModel()); err != nil {
		badRequest(c, err.Error())
		return false
	}
	if h.guardrails != nil {
		if err := h.guardrails.ValidateNames(c.Request.Context(), req.InputGuardrails, req.OutputGuardrails); err != nil {
			badRequest(c, err.Error())
			return false
		}
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

// agentConfigReq is the request body for both Create and Update; the two
// operations carry identical fields. id/created_at/updated_at are server-owned.
type agentConfigReq struct {
	Name          string `json:"name"`
	Instructions  string `json:"instructions"`
	Model         string `json:"model"`
	ProviderType  string `json:"provider_type"`
	AuthMode      string `json:"auth_mode"`
	APIKey        string `json:"api_key"`
	BaseURL       string `json:"base_url"`
	ModelSettings string `json:"model_settings"`
	ToolsJSON     string `json:"tools"`
	SkillsJSON    string `json:"skills"`
	HandoffsJSON  string `json:"handoffs"`

	MaxTurns               int    `json:"max_turns"`
	HandoffDescription     string `json:"handoff_description"`
	DisableToolChoiceReset bool   `json:"disable_tool_choice_reset"`
	ToolUseBehavior        string `json:"tool_use_behavior"`

	RetryEnabled   bool   `json:"retry_enabled"`
	RetryPolicy    string `json:"retry_policy"`
	FallbackModels string `json:"fallback_models"`

	InputGuardrails  string `json:"input_guardrails"`
	OutputGuardrails string `json:"output_guardrails"`
	OutputSchema     string `json:"output_schema"`

	UsePreviousResponseID bool   `json:"use_previous_response_id"`
	PromptID              string `json:"prompt_id"`
	PromptVersion         string `json:"prompt_version"`

	HandoffInputFilter   string `json:"handoff_input_filter"`
	MaxToolConcurrency   int    `json:"max_tool_concurrency"`
	ToolNotFoundBehavior string `json:"tool_not_found_behavior"`
	ErrorHandlers        string `json:"error_handlers"`

	CompactionEnabled   bool   `json:"compaction_enabled"`
	CompactionThreshold int    `json:"compaction_threshold"`
	CompactionWindow    int    `json:"compaction_window"`
	CompactionModel     string `json:"compaction_model"`
	CompactionPrompt    string `json:"compaction_prompt"`

	ApproveTools string `json:"approve_tools"`
}

func (r *agentConfigReq) toModel() *store.AgentConfig {
	return &store.AgentConfig{
		Name:                   r.Name,
		Instructions:           r.Instructions,
		Model:                  r.Model,
		ProviderType:           r.ProviderType,
		AuthMode:               r.AuthMode,
		APIKey:                 r.APIKey,
		BaseURL:                r.BaseURL,
		ModelSettings:          r.ModelSettings,
		ToolsJSON:              r.ToolsJSON,
		SkillsJSON:             r.SkillsJSON,
		HandoffsJSON:           r.HandoffsJSON,
		MaxTurns:               r.MaxTurns,
		HandoffDescription:     r.HandoffDescription,
		DisableToolChoiceReset: r.DisableToolChoiceReset,
		ToolUseBehavior:        r.ToolUseBehavior,
		RetryEnabled:           r.RetryEnabled,
		RetryPolicy:            r.RetryPolicy,
		FallbackModels:         r.FallbackModels,
		InputGuardrails:        r.InputGuardrails,
		OutputGuardrails:       r.OutputGuardrails,
		OutputSchema:           r.OutputSchema,
		UsePreviousResponseID:  r.UsePreviousResponseID,
		PromptID:               r.PromptID,
		PromptVersion:          r.PromptVersion,
		HandoffInputFilter:     r.HandoffInputFilter,
		MaxToolConcurrency:     r.MaxToolConcurrency,
		ToolNotFoundBehavior:   r.ToolNotFoundBehavior,
		ErrorHandlers:          r.ErrorHandlers,
		CompactionEnabled:      r.CompactionEnabled,
		CompactionThreshold:    r.CompactionThreshold,
		CompactionWindow:       r.CompactionWindow,
		CompactionModel:        r.CompactionModel,
		CompactionPrompt:       r.CompactionPrompt,
		ApproveTools:           r.ApproveTools,
	}
}

// Create persists a new agent configuration from the request body.
//
//	@Summary		Create agent
//	@Description	Secret fields (api_key, fallback_models[].api_key) are write-only: responses mask them with ********; sending the mask back keeps the stored value, "" clears it. use_previous_response_id is rejected (incompatible with the server-side session store), as are tool selections whose statically known tool names would collide.
//	@Tags			agents
//	@Accept			json
//	@Produce		json
//	@Param			agent	body		agentConfigReq	true	"Agent configuration"
//	@Success		201		{object}	store.AgentConfig
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents [post]
func (h *AgentConfigHandler) Create(c *gin.Context) {
	var req agentConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validateAgentConfig(c, &req) {
		return
	}
	ac := req.toModel()
	// There is no stored value yet, so a mask sentinel resolves to empty.
	ac.APIKey = resolveSecret(ac.APIKey, "")
	ac.FallbackModels = restoreFallbackModels(ac.FallbackModels, "")
	if err := h.store.Create(c.Request.Context(), ac); err != nil {
		saveError(c, err) // duplicate name -> 409
		return
	}
	sanitizeAgentConfig(ac)
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
//	@Description	Full replace. Secret fields are write-only: send back the ******** mask to keep the stored value, "" to clear it. use_previous_response_id is rejected (incompatible with the server-side session store), as are tool selections whose statically known tool names would collide.
//	@Tags			agents
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"Agent ID"
//	@Param			agent	body		agentConfigReq	true	"Agent configuration"
//	@Success		200		{object}	store.AgentConfig
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents/{id} [put]
func (h *AgentConfigHandler) Update(c *gin.Context) {
	var req agentConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validateAgentConfig(c, &req) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	ac := req.toModel()
	var prevKey, prevFallback string
	if prev, err := h.store.Get(ctx, id); err == nil {
		prevKey, prevFallback = prev.APIKey, prev.FallbackModels
	}
	ac.APIKey = resolveSecret(ac.APIKey, prevKey)
	ac.FallbackModels = restoreFallbackModels(ac.FallbackModels, prevFallback)
	if err := h.store.Update(ctx, id, ac); err != nil {
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
