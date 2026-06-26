package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// GuardrailHandler serves CRUD and catalog endpoints for guardrails.
type GuardrailHandler struct {
	store    *store.GuardrailStore
	resolver *bridge.GuardrailResolver
}

// NewGuardrailHandler returns a guardrail handler backed by the given store and resolver.
func NewGuardrailHandler(s *store.GuardrailStore, r *bridge.GuardrailResolver) *GuardrailHandler {
	return &GuardrailHandler{store: s, resolver: r}
}

// List responds with all available guardrails (stored + built-in).
func (h *GuardrailHandler) List(c *gin.Context) {
	c.JSON(http.StatusOK, h.resolver.ListGuardrails(c.Request.Context()))
}

// Create persists a new guardrail definition.
func (h *GuardrailHandler) Create(c *gin.Context) {
	var g store.Guardrail
	if err := c.ShouldBindJSON(&g); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if g.Name == "" || g.Type == "" || g.Mode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name, type, and mode are required"})
		return
	}
	if err := h.store.Create(c.Request.Context(), &g); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, g)
}

// Get responds with a single guardrail by ID.
func (h *GuardrailHandler) Get(c *gin.Context) {
	g, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, g)
}

// Update overwrites a guardrail definition.
func (h *GuardrailHandler) Update(c *gin.Context) {
	var g store.Guardrail
	if err := c.ShouldBindJSON(&g); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Update(c.Request.Context(), c.Param("id"), &g); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete removes a guardrail by ID.
func (h *GuardrailHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
