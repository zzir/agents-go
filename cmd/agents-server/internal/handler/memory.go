package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/memory"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// MemoryHandler serves memories: the configuration scopes under /memories,
// a session's own under /sessions/:id/memory. Who may read and write each
// scope is store.MemoryPolicies (invariant 64).
type MemoryHandler struct {
	store    *store.MemoryStore
	sessions *store.SessionStore
	agents   *store.AgentConfigStore
	entries  *store.EntryStore
}

// NewMemoryHandler returns a handler over the memory store; sessions and
// agents answer the per-scope ownership questions, entries a session's generation.
func NewMemoryHandler(memories *store.MemoryStore, sessions *store.SessionStore, agents *store.AgentConfigStore, entries *store.EntryStore) *MemoryHandler {
	return &MemoryHandler{store: memories, sessions: sessions, agents: agents, entries: entries}
}

// memoryReq is the request body for Create and Update.
type memoryReq struct {
	// ScopeKind is global, agent or session; ScopeID the agent or session id, empty for global.
	ScopeKind string `json:"scope_kind"`
	ScopeID   string `json:"scope_id"`
	// Key is unique within the scope; path-like, at most 200 characters.
	Key      string `json:"key"`
	Content  string `json:"content"`
	Metadata string `json:"metadata"`
}

func (r *memoryReq) validate() string {
	policy, ok := store.MemoryPolicyFor(r.ScopeKind)
	if !ok {
		return "scope_kind must be global, agent or session"
	}
	if r.ScopeKind == store.MemoryScopeGlobal && r.ScopeID != "" {
		return "a global memory has no scope_id"
	}
	if r.ScopeKind != store.MemoryScopeGlobal && r.ScopeID == "" {
		return "scope_id is required for an " + r.ScopeKind + " memory"
	}
	if err := memory.ValidKey(r.Key); err != nil {
		return err.Error()
	}
	if r.Content == "" {
		return "content is required"
	}
	if len(r.Content) > policy.MaxBytes {
		return fmt.Sprintf("content exceeds the %d-byte limit of a %s memory", policy.MaxBytes, r.ScopeKind)
	}
	return ""
}

// List responds with the configuration memories the caller may see.
//
//	@Summary		List memories
//	@Description	Global memories and the memories of agents visible to the caller. Session memory is read under /sessions/{id}/memory.
//	@Tags			memories
//	@Produce		json
//	@Param			scope_kind	query		string	false	"global or agent"
//	@Param			scope_id	query		string	false	"An agent id"
//	@Success		200			{array}		store.Memory
//	@Failure		400			{object}	ErrorResponse
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/memories [get]
func (h *MemoryHandler) List(c *gin.Context) {
	kind := c.Query("scope_kind")
	if kind == store.MemoryScopeSession {
		badRequest(c, "session memory is read under /sessions/{id}/memory")
		return
	}
	u, _ := server.CurrentUser(c)
	memories, err := h.store.ListConfig(c.Request.Context(), u.ID, u.Role == store.RoleAdmin, kind, c.Query("scope_id"))
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNilList(memories))
}

// Create writes a memory under its scope and key, replacing one that exists.
//
//	@Summary		Create memory
//	@Description	Global memory is an admin's to write, an agent's memory its editor's, a session's its owner's. An existing key is replaced.
//	@Tags			memories
//	@Accept			json
//	@Produce		json
//	@Param			memory	body		memoryReq	true	"Memory"
//	@Success		201		{object}	store.Memory
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/memories [post]
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
	m, ok := h.writable(c, req)
	if !ok {
		return
	}
	if err := h.store.Upsert(c.Request.Context(), m, nil); err != nil {
		memoryStoreError(c, err)
		return
	}
	created(c, m.ID, m)
}

// Get responds with one memory.
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
	if !h.readable(c, m) {
		return
	}
	c.JSON(http.StatusOK, m)
}

// Update replaces a memory's content and metadata.
//
//	@Summary		Update memory
//	@Description	A memory's scope and key are its identity and must match the row; only content and metadata change.
//	@Tags			memories
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string		true	"Memory ID"
//	@Param			memory	body		memoryReq	true	"Memory"
//	@Success		200		{object}	store.Memory
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/memories/{id} [put]
func (h *MemoryHandler) Update(c *gin.Context) {
	ctx := c.Request.Context()
	prev, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	var req memoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		badRequest(c, msg)
		return
	}
	if req.ScopeKind != prev.ScopeKind || req.ScopeID != prev.ScopeID || req.Key != prev.Key {
		badRequest(c, "scope_kind, scope_id and key identify the memory and cannot change; create another instead")
		return
	}
	m, ok := h.writable(c, req)
	if !ok {
		return
	}
	if err := h.store.Upsert(ctx, m, nil); err != nil {
		memoryStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// Delete removes a memory.
//
//	@Summary	Delete memory
//	@Tags		memories
//	@Param		id	path	string	true	"Memory ID"
//	@Success	204
//	@Failure	403	{object}	ErrorResponse
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/memories/{id} [delete]
func (h *MemoryHandler) Delete(c *gin.Context) {
	ctx := c.Request.Context()
	m, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if _, ok := h.writable(c, memoryReq{ScopeKind: m.ScopeKind, ScopeID: m.ScopeID, Key: m.Key, Content: m.Content}); !ok {
		return
	}
	if err := h.store.Delete(ctx, m.ID); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SessionMemoryInfo is one session memory without its content.
type SessionMemoryInfo struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Bytes     int       `json:"bytes"`
	WrittenBy string    `json:"written_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListSession lists a session's own memory, the keys and sizes.
//
//	@Summary	List session memory
//	@Tags		sessions
//	@Produce	json
//	@Param		id	path		string	true	"Session ID"
//	@Success	200	{array}		SessionMemoryInfo
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions/{id}/memory [get]
func (h *MemoryHandler) ListSession(c *gin.Context) {
	ctx := c.Request.Context()
	sess, err := gatedSession(c, h.sessions)
	if err != nil {
		storeError(c, err)
		return
	}
	ref, err := h.entries.RefFor(ctx, sess.ID)
	if err != nil {
		storeError(c, err)
		return
	}
	rows, err := h.store.ListScope(ctx, store.SessionMemoryScope(ref))
	if err != nil {
		internalError(c, err)
		return
	}
	out := make([]SessionMemoryInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionMemoryInfo{ID: r.ID, Key: r.Key, Bytes: len(r.Content), WrittenBy: r.WrittenBy, UpdatedAt: r.UpdatedAt})
	}
	c.JSON(http.StatusOK, out)
}

// ReadSession responds with one session memory in full.
//
//	@Summary	Read session memory
//	@Tags		sessions
//	@Produce	json
//	@Param		id	path		string	true	"Session ID"
//	@Param		key	path		string	true	"Memory key"
//	@Success	200	{object}	store.Memory
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions/{id}/memory/{key} [get]
func (h *MemoryHandler) ReadSession(c *gin.Context) {
	ctx := c.Request.Context()
	sess, err := gatedSession(c, h.sessions)
	if err != nil {
		storeError(c, err)
		return
	}
	ref, err := h.entries.RefFor(ctx, sess.ID)
	if err != nil {
		storeError(c, err)
		return
	}
	m, err := h.store.GetByKey(ctx, store.SessionMemoryScope(ref), strings.TrimPrefix(c.Param("key"), "/"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// writable checks the caller may write req's scope and builds the row to
// write; on refusal the response is already written. A session memory lands
// under the session's current generation.
func (h *MemoryHandler) writable(c *gin.Context, req memoryReq) (*store.Memory, bool) {
	ctx := c.Request.Context()
	u, ok := server.CurrentUser(c)
	if !ok {
		notFound(c)
		return nil, false
	}
	policy, _ := store.MemoryPolicyFor(req.ScopeKind)
	m := &store.Memory{
		ScopeKind: req.ScopeKind, ScopeID: req.ScopeID, Key: req.Key,
		Content: req.Content, Metadata: req.Metadata,
		WrittenBy: store.MemoryWrittenByUser, OwnerID: u.ID,
	}
	switch policy.Write {
	case store.MemoryWriteAdmin:
		if u.Role != store.RoleAdmin {
			abortError(c, http.StatusForbidden, protocol.CodeForbidden, "only an admin writes global memory")
			return nil, false
		}
	case store.MemoryWriteAgentEditor:
		ac, err := h.agents.Get(ctx, req.ScopeID)
		if err != nil {
			storeError(c, err)
			return nil, false
		}
		if !editableRow(c, ac.Scope, ac.OwnerID) {
			return nil, false
		}
	case store.MemoryWriteSessionOwner:
		sess, err := h.sessions.Get(ctx, req.ScopeID)
		if err != nil || sess.OwnerID != u.ID {
			notFound(c)
			return nil, false
		}
		ref, err := h.entries.RefFor(ctx, sess.ID)
		if err != nil {
			storeError(c, err)
			return nil, false
		}
		m.Gen = ref.Gen
	}
	return m, true
}

// readable checks the caller may read m; a refusal is a 404, as for any
// row the caller cannot see.
func (h *MemoryHandler) readable(c *gin.Context, m *store.Memory) bool {
	ctx := c.Request.Context()
	u, ok := server.CurrentUser(c)
	if !ok {
		notFound(c)
		return false
	}
	switch m.ScopeKind {
	case store.MemoryScopeAgent:
		ac, err := h.agents.Get(ctx, m.ScopeID)
		if err != nil {
			storeError(c, err)
			return false
		}
		return visibleRow(c, ac.Scope, ac.OwnerID)
	case store.MemoryScopeSession:
		sess, err := h.sessions.Get(ctx, m.ScopeID)
		if err != nil || sess.OwnerID != u.ID {
			notFound(c)
			return false
		}
	}
	return true
}

// memoryStoreError maps a policy refusal to 400 and an edit-rule refusal to 403.
func memoryStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrMemoryTooLarge), errors.Is(err, store.ErrMemoryLimit), errors.Is(err, store.ErrMemoryScope):
		badRequest(c, err.Error())
	case errors.Is(err, store.ErrMemoryForbidden):
		abortError(c, http.StatusForbidden, protocol.CodeForbidden, err.Error())
	default:
		storeError(c, err)
	}
}
