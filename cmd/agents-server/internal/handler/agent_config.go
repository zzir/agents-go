// Package handler implements the HTTP and WebSocket request handlers for the agents-server API.
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
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
	guardrails *guardrails.Resolver
	// providers, when set, lets save-time validation reject a provider_id that
	// names no row — the write side of referential integrity, whose read side
	// is ProviderStore.DeleteIfUnreferenced.
	providers *store.ProviderStore
	// skills backs the reference-scope validation of the skills selection.
	skills *store.SkillStore
}

// NewAgentConfigHandler returns a handler over the agent store and the three
// stores its validation reads (the MCP servers, providers and guardrails a
// config may name). Every one is required; a nil is a wiring error.
func NewAgentConfigHandler(s *store.AgentConfigStore, mcpServers *store.McpServerStore, providers *store.ProviderStore, skills *store.SkillStore, guardrails *guardrails.Resolver) *AgentConfigHandler {
	if s == nil || mcpServers == nil || providers == nil || skills == nil || guardrails == nil {
		panic("handler: NewAgentConfigHandler needs every store")
	}
	return &AgentConfigHandler{store: s, mcpServers: mcpServers, providers: providers, skills: skills, guardrails: guardrails}
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
	// An agent naming a provider that does not exist — or one its scope may
	// not reference (decisions §5.29) — would fail at run time with a confusing
	// error mid-stream; refuse at save.
	if ac.ProviderID != "" {
		pv, err := h.providers.Get(c.Request.Context(), ac.ProviderID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				badRequest(c, "provider_id names no provider")
			} else {
				internalError(c, err)
			}
			return false
		}
		if !store.RefVisible(pv.Scope, pv.OwnerID, ac.Scope, ac.OwnerID) {
			if !callerSees(c, pv.Scope, pv.OwnerID) {
				badRequest(c, "provider_id names no provider") // foreign private reads as absent
			} else {
				badRequest(c, refScopeError("provider", pv.Name, ac.Scope))
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
	spec, err := bridge.DecodeAgentSpec(ac)
	if err != nil {
		badRequest(c, err.Error())
		return false
	}
	// The referenced MCP servers, skills and handoff targets must be ones
	// this agent's scope may name. Missing ids stay TOLERATED here (deletes
	// are, and the run filters them loudly); only a visible-but-forbidden
	// reference is refused.
	for _, id := range spec.Tools {
		if ms, err := h.mcpServers.Get(c.Request.Context(), id); err == nil {
			if !store.RefVisible(ms.Scope, ms.OwnerID, ac.Scope, ac.OwnerID) {
				if !callerSees(c, ms.Scope, ms.OwnerID) {
					continue // reads as absent; the run drops it the same way
				}
				badRequest(c, refScopeError("MCP server", ms.Name, ac.Scope))
				return false
			}
		}
	}
	for _, id := range spec.Skills {
		if sk, err := h.skills.Get(c.Request.Context(), id); err == nil {
			if !store.RefVisible(sk.Scope, sk.OwnerID, ac.Scope, ac.OwnerID) {
				if !callerSees(c, sk.Scope, sk.OwnerID) {
					continue // reads as absent; the run drops it the same way
				}
				badRequest(c, refScopeError("skill", sk.Name, ac.Scope))
				return false
			}
		}
	}
	for _, id := range spec.Handoffs {
		if target, err := h.store.Get(c.Request.Context(), id); err == nil {
			if !store.RefVisible(target.Scope, target.OwnerID, ac.Scope, ac.OwnerID) {
				if !callerSees(c, target.Scope, target.OwnerID) {
					continue // reads as absent; the run drops it the same way
				}
				badRequest(c, refScopeError("handoff agent", target.Name, ac.Scope))
				return false
			}
		}
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
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	configs, err := store.ListVisibleOf(c.Request.Context(), h.store.CrudStore, ownerID, admin)
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
	if !stampCreateScope(c, &ac.Scope, &ac.OwnerID) {
		return
	}
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
	created(c, ac.ID, ac)
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
	if !visibleRow(c, ac.Scope, ac.OwnerID) {
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
	ctx := c.Request.Context()
	id := c.Param("id")
	cur, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if !editableRow(c, cur.Scope, cur.OwnerID) {
		return
	}
	// Scope and owner never move on an update (POST /:id/scope does); the
	// validation runs with the row's real scope.
	ac.Scope, ac.OwnerID = cur.Scope, cur.OwnerID
	if !h.validateAgentConfig(c, &ac) {
		return
	}
	// The masked fallback-model keys round-trip to their stored values inside
	// the store's transaction.
	// ownershipGuard re-checks the pair editableRow authorized against, now
	// inside the transaction: a transfer that landed since answers 409.
	err = h.store.Update(ctx, id, &ac, ownershipGuard(cur.Scope, cur.OwnerID,
		func(a *store.AgentConfig) (string, string) { return a.Scope, a.OwnerID },
		func(prev *store.AgentConfig) error {
			ac.Scope, ac.OwnerID = prev.Scope, prev.OwnerID
			ac.Resilience.FallbackModels = restoreFallbackModels(ac.Resilience.FallbackModels, prev.Resilience.FallbackModels)
			return nil
		}))
	if err != nil {
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
	cur, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if !deletableRow(c, cur.Scope, cur.OwnerID) {
		return
	}
	if err := store.DeleteOwnedBy(c.Request.Context(), h.store.CrudStore, c.Param("id"), cur.OwnerID); err != nil {
		saveError(c, err) // moved since the check -> 409
		return
	}
	c.Status(http.StatusNoContent)
}

// SetScope promotes an agent to global — after checking every reference it
// holds is global too — or demotes it back to its author's private set.
// Entities still referencing a demoted agent fail loudly at their next use.
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
func (h *AgentConfigHandler) SetScope(c *gin.Context) {
	scope, ok := bindScope(c)
	if !ok {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	ac, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if !scopeChangeAllowed(c, scope, ac.Scope, ac.OwnerID) {
		return
	}
	if sameScope(c, "agent", ac.Scope, scope) {
		return
	}
	// A promote re-runs the reference validation AS the target scope: a
	// global agent may only name global providers, servers, skills and
	// handoff targets.
	ac.Scope = scope
	if !h.validateAgentConfig(c, ac) {
		return
	}
	// The store re-checks the provider leg as the target scope inside its
	// transaction, so a demote landing between the validation above and this
	// write cannot leave a global agent on a private key.
	if err := h.store.SetScope(ctx, id, scope); err != nil {
		saveError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
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
func (h *AgentConfigHandler) SetOwner(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		badRequest(c, "user_id is required")
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	ac, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	// The references are re-validated AS THE NEW OWNER: handing over an agent
	// that names the old owner's private provider or MCP server would answer
	// 204 and then fail every run — the state a save refuses (decisions §5.29).
	// The provider leg re-checks again inside the store's transaction.
	ac.OwnerID = req.UserID
	if !h.validateAgentConfig(c, ac) {
		return
	}
	if err := h.store.TransferOwner(ctx, id, req.UserID); err != nil {
		switch {
		case errors.Is(err, store.ErrNoSuchUser):
			badRequest(c, "no such user")
		case errors.Is(err, store.ErrProviderScope):
			badRequest(c, "the new owner cannot see this agent's provider; repoint or publish it first")
		default:
			saveError(c, err)
		}
		return
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
}

// refScopeError words a refused cross-scope reference.
func refScopeError(kind, name, holderScope string) string {
	if holderScope == store.ScopeGlobal {
		return "a global agent can only reference global configuration; " + kind + " " + name + " is private"
	}
	return kind + " " + name + " belongs to another user"
}
