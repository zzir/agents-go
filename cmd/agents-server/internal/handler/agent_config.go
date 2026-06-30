// Package handler implements the HTTP and WebSocket request handlers for the agents-server API.
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// AgentConfigHandler serves CRUD endpoints for agent configurations.
type AgentConfigHandler struct {
	store *store.AgentConfigStore
}

// NewAgentConfigHandler returns a handler backed by the given store.
func NewAgentConfigHandler(s *store.AgentConfigStore) *AgentConfigHandler {
	return &AgentConfigHandler{store: s}
}

// List responds with all agent configurations.
func (h *AgentConfigHandler) List(c *gin.Context) {
	configs, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
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
		CompactionEnabled:      r.CompactionEnabled,
		CompactionThreshold:    r.CompactionThreshold,
		CompactionWindow:       r.CompactionWindow,
		CompactionModel:        r.CompactionModel,
		CompactionPrompt:       r.CompactionPrompt,
		ApproveTools:           r.ApproveTools,
	}
}

// Create persists a new agent configuration from the request body.
func (h *AgentConfigHandler) Create(c *gin.Context) {
	var req agentConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	ac := req.toModel()
	if err := h.store.Create(c.Request.Context(), ac); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, ac)
}

// Get responds with the agent configuration identified by the id path parameter.
func (h *AgentConfigHandler) Get(c *gin.Context) {
	ac, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, ac)
}

// Update overwrites the agent configuration identified by the id path parameter.
func (h *AgentConfigHandler) Update(c *gin.Context) {
	var req agentConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Update(c.Request.Context(), c.Param("id"), req.toModel()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete removes the agent configuration identified by the id path parameter.
func (h *AgentConfigHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
