package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// MemoryHandler serves CRUD endpoints for stored agent memories.
type MemoryHandler struct {
	store *store.MemoryStore
}

// NewMemoryHandler returns a handler backed by the given store.
func NewMemoryHandler(s *store.MemoryStore) *MemoryHandler {
	return &MemoryHandler{store: s}
}

// List responds with all stored memories.
func (h *MemoryHandler) List(c *gin.Context) {
	memories, err := h.store.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, memories)
}

// memoryReq is the request body for both Create and Update.
type memoryReq struct {
	AgentConfigID string `json:"agent_config_id"`
	Key           string `json:"key"`
	Content       string `json:"content"`
	Metadata      string `json:"metadata"`
}

func (r *memoryReq) toModel() *store.Memory {
	return &store.Memory{
		AgentConfigID: r.AgentConfigID,
		Key:           r.Key,
		Content:       r.Content,
		Metadata:      r.Metadata,
	}
}

// Create persists a new memory from the request body.
func (h *MemoryHandler) Create(c *gin.Context) {
	var req memoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}
	if req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content is required"})
		return
	}
	m := req.toModel()
	if err := h.store.Create(c.Request.Context(), m); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, m)
}

// Get responds with the memory identified by the id path parameter.
func (h *MemoryHandler) Get(c *gin.Context) {
	m, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, m)
}

// Update overwrites the memory identified by the id path parameter.
func (h *MemoryHandler) Update(c *gin.Context) {
	var req memoryReq
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

// Delete removes the memory identified by the id path parameter.
func (h *MemoryHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
