package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProviderRouteHandler serves CRUD endpoints for provider routes: a model-name
// prefix pointing at a provider. A route carries no credential of its own — it
// names the provider that holds one — so nothing here masks or round-trips a
// secret.
type ProviderRouteHandler struct {
	store *store.ProviderRouteStore
	// providers validates the referenced row exists, the write side of the
	// referential integrity ProviderStore.DeleteIfUnreferenced enforces.
	providers *store.ProviderStore
}

// NewProviderRouteHandler returns a handler backed by the given stores.
func NewProviderRouteHandler(s *store.ProviderRouteStore, providerStore *store.ProviderStore) *ProviderRouteHandler {
	return &ProviderRouteHandler{store: s, providers: providerStore}
}

// bind decodes and validates an incoming route body, reporting the failure
// itself.
func (h *ProviderRouteHandler) bind(c *gin.Context, pr *store.ProviderRoute) bool {
	if err := c.ShouldBindJSON(pr); err != nil {
		badRequest(c, err.Error())
		return false
	}
	pr.Prefix = strings.TrimSpace(pr.Prefix)
	if pr.Prefix == "" {
		badRequest(c, "prefix is required")
		return false
	}
	// The router matches the segment BEFORE the first "/" of a model name
	// exactly, so a prefix that itself contains "/" (or is whitespace) can never
	// be hit — refuse it rather than save a route that silently never works.
	if strings.Contains(pr.Prefix, "/") {
		badRequest(c, "prefix must not contain '/': it matches the segment before the first slash of a model name")
		return false
	}
	if pr.ProviderID == "" {
		badRequest(c, "provider_id is required")
		return false
	}
	if h.providers != nil {
		pv, err := h.providers.Get(c.Request.Context(), pr.ProviderID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				badRequest(c, "provider_id names no provider")
			} else {
				internalError(c, err)
			}
			return false
		}
		// A chatgpt_login provider cannot be routed: its OAuth token is fetched
		// through the full resolve path, which the router does not run, so the
		// route would silently never work. Refuse it at save rather than show a
		// dead "saved" route.
		if pv.AuthMode == providers.AuthModeChatGPTLogin {
			badRequest(c, "a chatgpt_login provider cannot be used through a route: its OAuth token only works on the direct path")
			return false
		}
	}
	return true
}

// List responds with all provider routes.
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
	c.JSON(http.StatusOK, routes)
}

// Get responds with the provider route identified by the id path parameter.
//
//	@Summary	Get provider route
//	@Tags		provider-routes
//	@Produce	json
//	@Param		id	path		string	true	"Provider route ID"
//	@Success	200	{object}	store.ProviderRoute
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/provider-routes/{id} [get]
func (h *ProviderRouteHandler) Get(c *gin.Context) {
	pr, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, pr)
}

// Create stores a new provider route.
//
//	@Summary		Create provider route
//	@Description	Routes every model name starting with prefix to the named provider.
//	@Tags			provider-routes
//	@Accept			json
//	@Produce		json
//	@Param			route	body		store.ProviderRoute	true	"Provider route"
//	@Success		201		{object}	store.ProviderRoute
//	@Failure		400		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"duplicate prefix"
//	@Security		BearerAuth
//	@Router			/provider-routes [post]
func (h *ProviderRouteHandler) Create(c *gin.Context) {
	var pr store.ProviderRoute
	if !h.bind(c, &pr) {
		return
	}
	pr.ID = "" // server-owned
	if err := h.store.Create(c.Request.Context(), &pr); err != nil {
		saveError(c, err) // duplicate prefix -> 409
		return
	}
	created(c, pr.ID, pr)
}

// Update overwrites the provider route identified by the id path parameter.
//
//	@Summary	Update provider route
//	@Tags		provider-routes
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string				true	"Provider route ID"
//	@Param		route	body		store.ProviderRoute	true	"Provider route"
//	@Success	200		{object}	store.ProviderRoute
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	409		{object}	ErrorResponse	"duplicate prefix"
//	@Security	BearerAuth
//	@Router		/provider-routes/{id} [put]
func (h *ProviderRouteHandler) Update(c *gin.Context) {
	var pr store.ProviderRoute
	if !h.bind(c, &pr) {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	if err := h.store.Update(ctx, id, &pr); err != nil {
		saveError(c, err) // duplicate prefix -> 409, not-found -> 404
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
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
