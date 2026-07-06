package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProviderRouteHandler serves CRUD endpoints for model provider routes.
type ProviderRouteHandler struct {
	store *store.ProviderRouteStore
}

// NewProviderRouteHandler returns a handler backed by the given store.
func NewProviderRouteHandler(s *store.ProviderRouteStore) *ProviderRouteHandler {
	return &ProviderRouteHandler{store: s}
}

// List responds with all provider routes, API keys masked.
//
//	@Summary	List provider routes
//	@Tags		provider-routes
//	@Produce	json
//	@Success	200	{array}		store.ProviderRoute
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/provider-routes [get]
func (h *ProviderRouteHandler) List(c *gin.Context) {
	routes, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range routes {
		routes[i].APIKey = maskSecret(routes[i].APIKey)
	}
	c.JSON(http.StatusOK, routes)
}

type providerRouteReq struct {
	Prefix  string `json:"prefix"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
}

// Get responds with the provider route identified by the id path parameter,
// API key masked.
//
//	@Summary	Get provider route
//	@Tags		provider-routes
//	@Produce	json
//	@Param		id	path		string	true	"Provider route ID"
//	@Success	200	{object}	store.ProviderRoute
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/provider-routes/{id} [get]
func (h *ProviderRouteHandler) Get(c *gin.Context) {
	pr, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	pr.APIKey = maskSecret(pr.APIKey)
	c.JSON(http.StatusOK, pr)
}

// Create persists a new provider route from the request body.
//
//	@Summary		Create provider route
//	@Description	Maps a model-name prefix to an API key and base URL. api_key is write-only (******** mask semantics).
//	@Tags			provider-routes
//	@Accept			json
//	@Produce		json
//	@Param			route	body		providerRouteReq	true	"Provider route"
//	@Success		201		{object}	store.ProviderRoute
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/provider-routes [post]
func (h *ProviderRouteHandler) Create(c *gin.Context) {
	var req providerRouteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.Prefix == "" {
		badRequest(c, "prefix is required")
		return
	}
	pr := &store.ProviderRoute{
		Prefix:  req.Prefix,
		APIKey:  resolveSecret(req.APIKey, ""),
		BaseURL: req.BaseURL,
	}
	if err := h.store.Create(c.Request.Context(), pr); err != nil {
		saveError(c, err) // duplicate prefix -> 409
		return
	}
	pr.APIKey = maskSecret(pr.APIKey)
	c.JSON(http.StatusCreated, pr)
}

// Update overwrites the provider route identified by the id path parameter
// and responds with the updated route. A masked api_key keeps the stored
// value.
//
//	@Summary	Update provider route
//	@Tags		provider-routes
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string				true	"Provider route ID"
//	@Param		route	body		providerRouteReq	true	"Provider route"
//	@Success	200		{object}	store.ProviderRoute
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/provider-routes/{id} [put]
func (h *ProviderRouteHandler) Update(c *gin.Context) {
	var req providerRouteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.Prefix == "" {
		badRequest(c, "prefix is required")
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	var prevKey string
	if prev, err := h.store.Get(ctx, id); err == nil {
		prevKey = prev.APIKey
	}
	pr := &store.ProviderRoute{
		Prefix:  req.Prefix,
		APIKey:  resolveSecret(req.APIKey, prevKey),
		BaseURL: req.BaseURL,
	}
	if err := h.store.Update(ctx, id, pr); err != nil {
		saveError(c, err) // duplicate prefix -> 409, not-found -> 404
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	updated.APIKey = maskSecret(updated.APIKey)
	c.JSON(http.StatusOK, updated)
}

// Delete removes the provider route identified by the id path parameter.
//
//	@Summary	Delete provider route
//	@Tags		provider-routes
//	@Param		id	path	string	true	"Provider route ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/provider-routes/{id} [delete]
func (h *ProviderRouteHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
