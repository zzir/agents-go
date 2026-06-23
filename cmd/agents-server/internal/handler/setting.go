package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// SettingHandler serves endpoints for reading and writing key-value settings.
type SettingHandler struct {
	store *store.SettingStore
}

// NewSettingHandler returns a handler backed by the given store.
func NewSettingHandler(s *store.SettingStore) *SettingHandler {
	return &SettingHandler{store: s}
}

// List responds with all settings.
func (h *SettingHandler) List(c *gin.Context) {
	settings, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, settings)
}

// Get responds with the setting identified by the key path parameter.
func (h *SettingHandler) Get(c *gin.Context) {
	st, err := h.store.Get(c.Request.Context(), c.Param("key"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, st)
}

type setSettingReq struct {
	Value string `json:"value"`
}

// Set writes the value for the setting identified by the key path parameter.
func (h *SettingHandler) Set(c *gin.Context) {
	var req setSettingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Set(c.Request.Context(), c.Param("key"), req.Value); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete removes the setting identified by the key path parameter.
func (h *SettingHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("key")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
