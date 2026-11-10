// Package handler implements the HTTP and WebSocket request handlers for the
// agents-server API.
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// AgentConfigHandler serves CRUD endpoints for agent configurations.
type AgentConfigHandler struct {
	store *store.AgentConfigStore
	// mcpServers resolves the MCP server ids the tools field names, at save time.
	mcpServers *store.McpServerStore
	// guardrails resolves guardrail names at save time.
	guardrails *guardrails.Resolver
	// providers resolves provider_id at save time (the read side is
	// ProviderStore.DeleteIfUnreferenced).
	providers *store.ProviderStore
	// skills backs the reference-scope validation of the skills selection.
	skills *store.SkillStore
}

// NewAgentConfigHandler returns a handler over the agent store and the stores
// its validation reads. Every argument is required; a nil is a wiring error.
func NewAgentConfigHandler(s *store.AgentConfigStore, mcpServers *store.McpServerStore, providers *store.ProviderStore, skills *store.SkillStore, guardrails *guardrails.Resolver) *AgentConfigHandler {
	if s == nil || mcpServers == nil || providers == nil || skills == nil || guardrails == nil {
		panic("handler: NewAgentConfigHandler needs every store")
	}
	return &AgentConfigHandler{store: s, mcpServers: mcpServers, providers: providers, skills: skills, guardrails: guardrails}
}

// avatarPath is the shape of a built-in avatar reference — a same-origin path
// into the catalog shipped with the UI, never an arbitrary URL.
var avatarPath = regexp.MustCompile(`^/avatars/[A-Za-z0-9_-]+\.svg$`)

// validateAgentConfig checks a Create/Update body against what a run would
// otherwise hit at run time.
func (h *AgentConfigHandler) validateAgentConfig(c *gin.Context, ac *store.AgentConfig) error {
	ctx := c.Request.Context()
	if ac.Name == "" {
		return badRequest("name is required")
	}
	if err := nameFits(ac.Name); err != nil {
		return err
	}
	// No provider ships a default model: an empty one is refused at save.
	if ac.Model == "" {
		return badRequest("model is required")
	}
	// Only the built-in catalog: the CSP blocks an external image URL — invariant 50.
	if ac.Avatar != "" && !avatarPath.MatchString(ac.Avatar) {
		return badRequest("avatar must be a built-in path (/avatars/<name>.svg) or empty")
	}
	// A provider that does not exist, or that this scope may not reference,
	// is refused at save — decisions §5.29.
	if ac.ProviderID != "" {
		if err := h.providerRef(c, ac, ac.ProviderID, "provider_id"); err != nil {
			return err
		}
	}
	if err := bridge.ValidateAgentToolNames(ctx, h.mcpServers, ac.Tools); err != nil {
		return badRequest(err.Error())
	}
	// The same decode backs the build: what does not parse is refused here.
	if _, err := bridge.DecodeAgentSpec(ac); err != nil {
		return badRequest(err.Error())
	}
	// A fallback provider follows the primary's reference rule; an endpoint-only
	// entry is checked when the run resolves it.
	for i, e := range ac.Resilience.FallbackModels {
		if e.ProviderID == "" {
			continue
		}
		if err := h.providerRef(c, ac, e.ProviderID, fmt.Sprintf("fallback_models[%d].provider_id", i)); err != nil {
			return err
		}
	}
	// MCP servers, skills and handoff targets must be ones this scope may name;
	// a missing id, or one the caller cannot see, is dropped by the run the
	// same way — invariant 13.
	for _, id := range ac.Tools {
		if ms, err := h.mcpServers.Get(ctx, id); err == nil {
			if reason, _ := refRefusal(c, "MCP server", ms.Name, ms.Scope, ms.OwnerID, ac.Scope, ac.OwnerID); reason != "" {
				return badRequest(reason)
			}
		}
	}
	for _, id := range ac.Skills {
		if sk, err := h.skills.Get(ctx, id); err == nil {
			if reason, _ := refRefusal(c, "skill", sk.Name, sk.Scope, sk.OwnerID, ac.Scope, ac.OwnerID); reason != "" {
				return badRequest(reason)
			}
		}
	}
	for _, id := range ac.Handoffs {
		if target, err := h.store.Get(ctx, id); err == nil {
			if reason, _ := refRefusal(c, "handoff agent", target.Name, target.Scope, target.OwnerID, ac.Scope, ac.OwnerID); reason != "" {
				return badRequest(reason)
			}
		}
	}
	if err := h.guardrails.ValidateNames(ctx, ac.Guardrails.Guardrails); err != nil {
		return badRequest(err.Error())
	}
	return nil
}

// providerRef checks that the agent may name provider id; field words the
// refusal. A provider the caller cannot see reads as absent.
func (h *AgentConfigHandler) providerRef(c *gin.Context, ac *store.AgentConfig, id, field string) error {
	absent := badRequest(field + " names no provider")
	pv, err := h.providers.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return absent
		}
		return err
	}
	switch reason, hidden := refRefusal(c, "provider", pv.Name, pv.Scope, pv.OwnerID, ac.Scope, ac.OwnerID); {
	case hidden:
		return absent
	case reason != "":
		return badRequest(reason)
	}
	return nil
}

// bindAgentConfig reads a Create/Update body. A fallback entry must name a
// provider — decisions §5.69.
func bindAgentConfig(c *gin.Context, ac *store.AgentConfig) error {
	if err := c.ShouldBindJSON(ac); err != nil {
		return badRequest(err.Error())
	}
	if i := ac.Resilience.FallbackModels.InlineKeyAt(); i >= 0 {
		return badRequest(fmt.Sprintf("fallback_models[%d].api_key: a key lives on a provider; name one by provider_id", i))
	}
	for i, e := range ac.Resilience.FallbackModels {
		if e.ProviderID == "" {
			return badRequest(fmt.Sprintf("fallback_models[%d].provider_id is required", i))
		}
		if e.ProviderType != "" || e.BaseURL != "" {
			return badRequest(fmt.Sprintf("fallback_models[%d]: provider_type and base_url are read-only; the provider row carries the endpoint", i))
		}
	}
	return nil
}

// List responds with all agent configurations.
//
//	@Summary	List agents
//	@Tags		agents
//	@Produce	json
//	@Success	200	{array}		store.AgentConfig
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/agents [get]
func (h *AgentConfigHandler) List(c *gin.Context) error {
	configs, err := listVisible(c, h.store.CrudStore)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, configs)
	return nil
}

// Create persists a new agent configuration from the request body.
//
//	@Summary		Create agent
//	@Description	No secret lives here: the credential is on the agent's provider, and each resilience.fallback_models entry names a provider by provider_id (an api_key in an entry is 400). Tool selections whose statically known tool names would collide are rejected.
//	@Tags			agents
//	@Accept			json
//	@Produce		json
//	@Param			agent	body		store.AgentConfig	true	"Agent configuration"
//	@Success		201		{object}	store.AgentConfig
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents [post]
func (h *AgentConfigHandler) Create(c *gin.Context) error {
	var ac store.AgentConfig
	if err := bindAgentConfig(c, &ac); err != nil {
		return err
	}
	// id and timestamps are server-owned: BeforeAppendModel stamps the zero ones.
	ac.ID, ac.CreatedAt = "", time.Time{}
	if err := stampCreateScope(c, &ac.Scope, &ac.OwnerID); err != nil {
		return err
	}
	// A new agent's approval mode is explicit.
	if ac.Approval.Mode == "" {
		ac.Approval.Mode = store.ApprovalModeNever
	}
	if err := h.validateAgentConfig(c, &ac); err != nil {
		return err
	}
	if err := h.store.Create(c.Request.Context(), &ac); err != nil {
		return err // duplicate name -> 409
	}
	created(c, ac.ID, ac)
	return nil
}

// Get responds with the agent configuration identified by the id path
// parameter.
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
func (h *AgentConfigHandler) Get(c *gin.Context) error {
	ac, err := gatedRow(c, h.store.CrudStore, agentScope, visibleRow)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, ac)
	return nil
}

// Update overwrites the agent configuration identified by the id path parameter.
//
//	@Summary		Update agent
//	@Description	Full replace. Each resilience.fallback_models entry names a provider by provider_id; an entry from before provider_id (provider_type/base_url, read-only) is sent back as a provider_id or dropped. Tool selections whose statically known tool names would collide are rejected.
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
func (h *AgentConfigHandler) Update(c *gin.Context) error {
	var ac store.AgentConfig
	if err := bindAgentConfig(c, &ac); err != nil {
		return err
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	cur, err := gatedRow(c, h.store.CrudStore, agentScope, editableRow)
	if err != nil {
		return err
	}
	// Scope and owner never move on an update (POST /:id/scope does); the
	// validation runs with the row's real scope.
	ac.Scope, ac.OwnerID = cur.Scope, cur.OwnerID
	if err := h.validateAgentConfig(c, &ac); err != nil {
		return err
	}
	// ownershipGuard turns a transfer that landed since into 409.
	err = h.store.Update(ctx, id, &ac, ownershipGuard(cur.Scope, cur.OwnerID, agentScope,
		func(prev *store.AgentConfig) error {
			ac.Scope, ac.OwnerID = prev.Scope, prev.OwnerID
			return nil
		}))
	if err != nil {
		return err // duplicate name -> 409, not-found -> 404
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, updated)
	return nil
}

// Delete removes the agent configuration identified by the id path parameter.
//
//	@Summary		Delete agent
//	@Description	The memory scoped to the agent and the triggers that fire it are deleted with it.
//	@Tags			agents
//	@Param			id	path	string	true	"Agent ID"
//	@Success		204	"deleted"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents/{id} [delete]
func (h *AgentConfigHandler) Delete(c *gin.Context) error {
	if err := deleteOwned(c, h.store.CrudStore, agentScope, h.store.DeleteOwnedBy); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

// SetScope promotes an agent to global (every reference must be global too)
// or demotes it back to its author's private set — decisions §5.29.
//
//	@Summary	Change an agent's scope
//	@Tags		agents
//	@Accept		json
//	@Param		id		path	string		true	"Agent ID"
//	@Param		scope	body	scopeReq	true	"global or private"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"a promote holds non-global references"
//	@Failure	409	{object}	ErrorResponse	"name collision in the target scope"
//	@Security	BearerAuth
//	@Router		/agents/{id}/scope [post]
func (h *AgentConfigHandler) SetScope(c *gin.Context) error {
	// A promote re-runs the reference validation as the target scope; the store
	// re-checks the provider leg inside its transaction (a racing demote).
	return setScope(c, h.store.CrudStore, "agent", agentScope,
		func(ac *store.AgentConfig, target string) error {
			ac.Scope = target
			return h.validateAgentConfig(c, ac)
		},
		func(ctx context.Context, id, scope, _ string) error { return h.store.SetScope(ctx, id, scope) })
}

// SetOwner transfers the agent to another account (admin).
//
//	@Summary	Reassign an agent's owner (admin)
//	@Tags		agents
//	@Accept		json
//	@Param		id		path	string			true	"Agent ID"
//	@Param		body	body	SetOwnerRequest	true	"The new owner"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"malformed body, or no such user"
//	@Failure	409	{object}	ErrorResponse	"name collision in the target owner's namespace"
//	@Security	BearerAuth
//	@Router		/agents/{id}/owner [put]
func (h *AgentConfigHandler) SetOwner(c *gin.Context) error {
	return setOwner(c, func(ctx context.Context, id, userID string) error {
		ac, err := h.store.Get(ctx, id)
		if err != nil {
			return err
		}
		// References are re-validated as the new owner — decisions §5.29.
		ac.OwnerID = userID
		if err := h.validateAgentConfig(c, ac); err != nil {
			return err
		}
		err = h.store.TransferOwner(ctx, id, userID)
		if errors.Is(err, store.ErrProviderScope) {
			return badRequest("the new owner cannot see this agent's provider; repoint or publish it first")
		}
		return err
	})
}

// agentScope reads an agent's (scope, owner) pair for the scoped-CRUD gates.
func agentScope(a *store.AgentConfig) (string, string) { return a.Scope, a.OwnerID }
