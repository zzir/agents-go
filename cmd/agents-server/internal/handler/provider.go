package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProviderHandler serves CRUD endpoints for provider endpoints and their
// credentials — the ONLY surface a model-API key crosses.
type ProviderHandler struct {
	store *store.ProviderStore
}

// NewProviderHandler returns a handler backed by the given store.
func NewProviderHandler(s *store.ProviderStore) *ProviderHandler {
	return &ProviderHandler{store: s}
}

// providerReq is the request body for Create and Update; the id, the
// timestamps and the ChatGPT token are the server's.
type providerReq struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	AuthMode string `json:"auth_mode,omitempty"`
	// APIKey is write-only: the ******** mask keeps the stored key.
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// Scope on create only: "global" (admin) or the "private" default; an
	// update never moves it (POST /:id/scope does).
	Scope string `json:"scope,omitempty"`
}

func (r *providerReq) toModel() *store.Provider {
	return &store.Provider{Name: r.Name, Type: r.Type, AuthMode: r.AuthMode, APIKey: r.APIKey, BaseURL: r.BaseURL, Scope: r.Scope}
}

// bind decodes and validates an incoming provider body, reporting the failure
// itself. Type and auth mode are the provider registry's answer.
func (h *ProviderHandler) bind(c *gin.Context) (*store.Provider, bool) {
	var req providerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	pv := req.toModel()
	if err := store.NormalizeProvider(pv); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	if err := providers.Validate(pv); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	return pv, true
}

// List responds with every provider, keys masked.
//
//	@Summary	List providers
//	@Tags		providers
//	@Produce	json
//	@Success	200	{array}		store.Provider
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/providers [get]
func (h *ProviderHandler) List(c *gin.Context) {
	list, ok := listVisible(c, h.store.CrudStore)
	if !ok {
		return
	}
	for i := range list {
		sanitizeProvider(&list[i])
	}
	c.JSON(http.StatusOK, list)
}

// Get responds with one provider, key masked.
//
//	@Summary	Get a provider
//	@Tags		providers
//	@Produce	json
//	@Param		id	path		string	true	"Provider ID"
//	@Success	200	{object}	store.Provider
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/providers/{id} [get]
func (h *ProviderHandler) Get(c *gin.Context) {
	pv, ok := gatedRow(c, h.store.CrudStore, providerScope, visibleRow)
	if !ok {
		return
	}
	sanitizeProvider(pv)
	c.JSON(http.StatusOK, pv)
}

// providerScope reads a provider's (scope, owner) pair for the scoped-CRUD gates.
func providerScope(p *store.Provider) (string, string) { return p.Scope, p.OwnerID }

// scopeReq is the body of every POST /:id/scope — the promotion/demotion act
// (decisions §5.29): promote is admin-only, demote is the admin's or the owner's.
type scopeReq struct {
	Scope string `json:"scope" binding:"required"`
}

// bindScope decodes and validates a scope-change body.
func bindScope(c *gin.Context) (string, bool) {
	var req scopeReq
	if err := c.ShouldBindJSON(&req); err != nil || (req.Scope != store.ScopeGlobal && req.Scope != store.ScopePrivate) {
		badRequest(c, `scope must be "global" or "private"`)
		return "", false
	}
	return req.Scope, true
}

// sameScope refuses a /scope request naming the scope the row already holds
// (decisions §5.29). Writes the 409 and returns true when refused.
func sameScope(c *gin.Context, kind, current, requested string) bool {
	if current == requested {
		conflict(c, kind+" is already "+requested)
		return true
	}
	return false
}

// SetScope promotes a provider to global or demotes it back to its author's
// private set; DemoteToPrivate carries the foreign-reference guard.
//
//	@Summary	Change a provider's scope
//	@Tags		providers
//	@Accept		json
//	@Param		id		path	string		true	"Provider ID"
//	@Param		scope	body	scopeReq	true	"global or private"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"name collision in the target scope, or referencing agents block the demote"
//	@Security	BearerAuth
//	@Router		/providers/{id}/scope [post]
func (h *ProviderHandler) SetScope(c *gin.Context) {
	scope, ok := bindScope(c)
	if !ok {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	pv, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if !scopeChangeAllowed(c, scope, pv.Scope, pv.OwnerID) {
		return
	}
	if sameScope(c, "provider", pv.Scope, scope) {
		return
	}
	if scope == store.ScopePrivate {
		refs, err := h.store.DemoteToPrivate(ctx, id)
		if err != nil {
			saveError(c, err) // name collision in the target scope -> 409
			return
		}
		if refs > 0 {
			conflict(c, fmt.Sprintf("%d agent(s) outside the owner's private set still reference this provider; repoint them first", refs))
			return
		}
		c.Status(http.StatusNoContent)
		return
	}
	if err := store.SetScopeOf(ctx, h.store.CrudStore, id, scope, pv.OwnerID); err != nil {
		saveError(c, err) // name collision in the target scope -> 409
		return
	}
	c.Status(http.StatusNoContent)
}

// SetOwner transfers the provider — credential included — to another account
// (admin). Refused while the move would strand an agent that references it,
// the guard a demote carries for the same reason.
//
//	@Summary	Reassign a provider's owner (admin)
//	@Tags		providers
//	@Accept		json
//	@Param		id		path	string			true	"Provider ID"
//	@Param		body	body	SetOwnerRequest	true	"The new owner"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"malformed body, or no such user"
//	@Failure	409	{object}	ErrorResponse	"name collision, or referencing agents block the transfer"
//	@Security	BearerAuth
//	@Router		/providers/{id}/owner [put]
func (h *ProviderHandler) SetOwner(c *gin.Context) {
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		badRequest(c, "user_id is required")
		return
	}
	refs, err := h.store.TransferOwner(c.Request.Context(), c.Param("id"), req.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNoSuchUser) {
			badRequest(c, "no such user")
			return
		}
		saveError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, fmt.Sprintf("%d agent(s) outside the new owner's private set reference this provider; repoint them first", refs))
		return
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
}

// Create stores a new provider.
//
//	@Summary	Create a provider
//	@Tags		providers
//	@Accept		json
//	@Produce	json
//	@Param		provider	body		providerReq	true	"Provider"
//	@Success	201			{object}	store.Provider
//	@Failure	400			{object}	ErrorResponse
//	@Failure	409			{object}	ErrorResponse	"duplicate name"
//	@Security	BearerAuth
//	@Router		/providers [post]
func (h *ProviderHandler) Create(c *gin.Context) {
	pv, ok := h.bind(c)
	if !ok {
		return
	}
	if !stampCreateScope(c, &pv.Scope, &pv.OwnerID) {
		return
	}
	// There is no stored value yet, so a mask sentinel resolves to empty.
	pv.APIKey = resolveSecret(pv.APIKey, "")
	if err := h.store.Create(c.Request.Context(), pv); err != nil {
		saveError(c, err) // duplicate name -> 409
		return
	}
	sanitizeProvider(pv)
	created(c, pv.ID, pv)
}

// Update overwrites a provider.
//
//	@Summary		Update a provider
//	@Description	A masked api_key keeps the stored one — but only while the destination (type + base_url) is unchanged, since the stored key belongs to that destination.
//	@Tags			providers
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string		true	"Provider ID"
//	@Param			provider	body		providerReq	true	"Provider"
//	@Success		200			{object}	store.Provider
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"duplicate name"
//	@Security		BearerAuth
//	@Router			/providers/{id} [put]
func (h *ProviderHandler) Update(c *gin.Context) {
	pv, ok := h.bind(c)
	if !ok {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	cur, ok := gatedRow(c, h.store.CrudStore, providerScope, editableRow)
	if !ok {
		return
	}
	// The mask resolves against the stored row inside the transaction, only
	// for the destination the key was stored for — invariant 9.
	err := h.store.Update(ctx, id, pv, ownershipGuard(cur.Scope, cur.OwnerID, providerScope,
		func(prev *store.Provider) error {
			pv.Scope, pv.OwnerID = prev.Scope, prev.OwnerID
			if pv.APIKey == SecretMask && prev.APIKey != "" &&
				credentialTargetChanged(prev.Type, prev.BaseURL, pv.Type, pv.BaseURL) {
				return badRequestError("type or base_url changed: the stored api_key belongs to the previous destination — replace it or clear it")
			}
			pv.APIKey = resolveSecret(pv.APIKey, prev.APIKey)
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
	sanitizeProvider(updated)
	c.JSON(http.StatusOK, updated)
}

// Delete removes a provider nothing references.
//
//	@Summary		Delete a provider
//	@Description	Refuses with 409 while an agent still references it — repoint or delete those first.
//	@Tags			providers
//	@Param			id	path	string	true	"Provider ID"
//	@Success		204
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"still referenced"
//	@Security		BearerAuth
//	@Router			/providers/{id} [delete]
func (h *ProviderHandler) Delete(c *gin.Context) {
	cur, ok := gatedRow(c, h.store.CrudStore, providerScope, deletableRow)
	if !ok {
		return
	}
	refs, err := h.store.DeleteIfUnreferenced(c.Request.Context(), c.Param("id"), cur.OwnerID)
	if err != nil {
		storeError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, fmt.Sprintf("%d agent(s) still use this provider; repoint them first", refs))
		return
	}
	c.Status(http.StatusNoContent)
}
