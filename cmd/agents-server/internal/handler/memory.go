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
//
//	@Summary	List memories
//	@Tags		memories
//	@Produce	json
//	@Success	200	{array}		store.Memory
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/memories [get]
func (h *MemoryHandler) List(c *gin.Context) {
	memories, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
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

func (r *memoryReq) validate() string {
	if r.Key == "" {
		return "key is required"
	}
	if r.Content == "" {
		return "content is required"
	}
	return ""
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
//
//	@Summary	Create memory
//	@Tags		memories
//	@Accept		json
//	@Produce	json
//	@Param		memory	body		memoryReq	true	"Memory; agent_config_id scopes it to one agent, empty means global"
//	@Success	201		{object}	store.Memory
//	@Failure	400		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/memories [post]
func (h *MemoryHandler) Create(c *gin.Context) {
	var req memoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		badRequest(c, msg)
		return
	}
	m := req.toModel()
	if err := h.store.Create(c.Request.Context(), m); err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, m)
}

// Get responds with the memory identified by the id path parameter.
//
//	@Summary	Get memory
//	@Tags		memories
//	@Produce	json
//	@Param		id	path		string	true	"Memory ID"
//	@Success	200	{object}	store.Memory
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/memories/{id} [get]
func (h *MemoryHandler) Get(c *gin.Context) {
	m, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// Update overwrites the memory identified by the id path parameter and
// responds with the updated memory.
//
//	@Summary	Update memory
//	@Tags		memories
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string		true	"Memory ID"
//	@Param		memory	body		memoryReq	true	"Memory"
//	@Success	200		{object}	store.Memory
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/memories/{id} [put]
func (h *MemoryHandler) Update(c *gin.Context) {
	var req memoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		badRequest(c, msg)
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	if err := h.store.Update(ctx, id, req.toModel()); err != nil {
		storeError(c, err)
		return
	}
	m, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// Delete removes the memory identified by the id path parameter.
//
//	@Summary	Delete memory
//	@Tags		memories
//	@Param		id	path	string	true	"Memory ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/memories/{id} [delete]
func (h *MemoryHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
