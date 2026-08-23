package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProviderHandler serves CRUD endpoints for provider endpoints and their
// credentials. It is the ONLY surface a model-API key crosses: agents and
// routes reference a provider by id, so no other body carries one.
type ProviderHandler struct {
	store *store.ProviderStore
}

// NewProviderHandler returns a handler backed by the given store.
func NewProviderHandler(s *store.ProviderStore) *ProviderHandler {
	return &ProviderHandler{store: s}
}

// providerReq is the request body for Create and Update: what a client may
// set. The id, the timestamps and the ChatGPT token (the OAuth flow's) are
// the server's.
type providerReq struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	AuthMode string `json:"auth_mode,omitempty"`
	// APIKey is write-only: the ******** mask keeps the stored key.
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

func (r *providerReq) toModel() *store.Provider {
	return &store.Provider{Name: r.Name, Type: r.Type, AuthMode: r.AuthMode, APIKey: r.APIKey, BaseURL: r.BaseURL}
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
	list, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
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
	pv, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	sanitizeProvider(pv)
	c.JSON(http.StatusOK, pv)
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
	// The mask resolves against the stored row inside the store's transaction,
	// and only for the destination the key was stored for — README invariant 9.
	err := h.store.Update(ctx, id, pv, func(prev *store.Provider) error {
		if pv.APIKey == SecretMask && prev.APIKey != "" &&
			credentialTargetChanged(prev.Type, prev.BaseURL, pv.Type, pv.BaseURL) {
			return badRequestError("type or base_url changed: the stored api_key belongs to the previous destination — replace it or clear it")
		}
		pv.APIKey = resolveSecret(pv.APIKey, prev.APIKey)
		return nil
	})
	if err != nil {
		saveError(c, err) // duplicate name -> 409, not-found -> 404, routed chatgpt_login -> 400
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
//	@Description	Refuses with 409 while an agent or a provider route still references it — repoint or delete those first.
//	@Tags			providers
//	@Param			id	path	string	true	"Provider ID"
//	@Success		204
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"still referenced"
//	@Security		BearerAuth
//	@Router			/providers/{id} [delete]
func (h *ProviderHandler) Delete(c *gin.Context) {
	refs, err := h.store.DeleteIfUnreferenced(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, fmt.Sprintf("%d agent(s) or route(s) still use this provider; repoint them first", refs))
		return
	}
	c.Status(http.StatusNoContent)
}
