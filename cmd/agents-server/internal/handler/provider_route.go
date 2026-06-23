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

// List responds with all provider routes.
func (h *ProviderRouteHandler) List(c *gin.Context) {
	routes, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, routes)
}

type providerRouteReq struct {
	Prefix  string `json:"prefix"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
}

// Create persists a new provider route from the request body.
func (h *ProviderRouteHandler) Create(c *gin.Context) {
	var req providerRouteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Prefix == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prefix is required"})
		return
	}
	pr := &store.ProviderRoute{
		Prefix:  req.Prefix,
		APIKey:  req.APIKey,
		BaseURL: req.BaseURL,
	}
	if err := h.store.Create(c.Request.Context(), pr); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, pr)
}

// Update overwrites the provider route identified by the id path parameter.
func (h *ProviderRouteHandler) Update(c *gin.Context) {
	var req providerRouteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pr := &store.ProviderRoute{
		Prefix:  req.Prefix,
		APIKey:  req.APIKey,
		BaseURL: req.BaseURL,
	}
	if err := h.store.Update(c.Request.Context(), c.Param("id"), pr); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete removes the provider route identified by the id path parameter.
func (h *ProviderRouteHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
