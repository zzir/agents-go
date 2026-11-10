package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/memory"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// MemoryHandler serves memories: the configuration scopes under /memories, a
// session's own under /sessions/:id/memory; the rules are store.MemoryPolicies
// (invariant 64).
type MemoryHandler struct {
	store    *store.MemoryStore
	sessions *store.SessionStore
	agents   *store.AgentConfigStore
}

// NewMemoryHandler returns a handler over the memory store; sessions and
// agents answer the per-scope ownership questions.
func NewMemoryHandler(memories *store.MemoryStore, sessions *store.SessionStore, agents *store.AgentConfigStore) *MemoryHandler {
	return &MemoryHandler{store: memories, sessions: sessions, agents: agents}
}

// memoryReq is the request body for Create and Update.
type memoryReq struct {
	// ScopeKind is global, agent or session; ScopeID the agent or session id,
	// empty for global.
	ScopeKind string `json:"scope_kind"`
	ScopeID   string `json:"scope_id"`
	// Key is unique within the scope; path-like, at most 200 characters.
	Key      string `json:"key"`
	Content  string `json:"content"`
	Metadata string `json:"metadata"`
}

func (r *memoryReq) validate() error {
	policy, ok := store.MemoryPolicies[r.ScopeKind]
	if !ok {
		return badRequest("scope_kind must be global, agent or session")
	}
	if r.ScopeKind == store.MemoryScopeGlobal && r.ScopeID != "" {
		return badRequest("a global memory has no scope_id")
	}
	if r.ScopeKind != store.MemoryScopeGlobal && r.ScopeID == "" {
		return badRequest("scope_id is required for an " + r.ScopeKind + " memory")
	}
	if err := memory.ValidKey(r.Key); err != nil {
		return badRequest(err.Error())
	}
	if r.Content == "" {
		return badRequest("content is required")
	}
	if len(r.Content) > policy.MaxBytes {
		return badRequest(fmt.Sprintf("content exceeds the %d-byte limit of a %s memory", policy.MaxBytes, r.ScopeKind))
	}
	return nil
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
func (h *MemoryHandler) List(c *gin.Context) error {
	kind := c.Query("scope_kind")
	if kind == store.MemoryScopeSession {
		return badRequest("session memory is read under /sessions/{id}/memory")
	}
	u, _ := server.CurrentUser(c)
	memories, err := h.store.ListConfig(c.Request.Context(), u.ID, u.Role == store.RoleAdmin, kind, c.Query("scope_id"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, nonNilList(memories))
	return nil
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
func (h *MemoryHandler) Create(c *gin.Context) error {
	var req memoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		return badRequest(err.Error())
	}
	if err := req.validate(); err != nil {
		return err
	}
	m, err := h.writable(c, req)
	if err != nil {
		return err
	}
	if err := h.store.Upsert(c.Request.Context(), m, nil); err != nil {
		return memoryStoreError(err)
	}
	created(c, m.ID, m)
	return nil
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
func (h *MemoryHandler) Get(c *gin.Context) error {
	m, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	if err := h.readable(c, m); err != nil {
		return err
	}
	c.JSON(http.StatusOK, m)
	return nil
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
func (h *MemoryHandler) Update(c *gin.Context) error {
	ctx := c.Request.Context()
	prev, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	var req memoryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		return badRequest(err.Error())
	}
	if err := req.validate(); err != nil {
		return err
	}
	if req.ScopeKind != prev.ScopeKind || req.ScopeID != prev.ScopeID || req.Key != prev.Key {
		return badRequest("scope_kind, scope_id and key identify the memory and cannot change; create another instead")
	}
	m, err := h.writable(c, req)
	if err != nil {
		return err
	}
	if err := h.store.Upsert(ctx, m, nil); err != nil {
		return memoryStoreError(err)
	}
	c.JSON(http.StatusOK, m)
	return nil
}

// Delete removes a memory.
//
//	@Summary		Delete memory
//	@Description	Who may write the scope may delete; a memory whose agent has been deleted is an admin's to delete.
//	@Tags			memories
//	@Param			id	path	string	true	"Memory ID"
//	@Success		204
//	@Failure		403	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/memories/{id} [delete]
func (h *MemoryHandler) Delete(c *gin.Context) error {
	ctx := c.Request.Context()
	m, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if h.orphaned(ctx, m) {
		if u, _ := server.CurrentUser(c); u.Role != store.RoleAdmin {
			return errNotFound
		}
	} else if _, err := h.writable(c, memoryReq{ScopeKind: m.ScopeKind, ScopeID: m.ScopeID, Key: m.Key, Content: m.Content}); err != nil {
		return err
	}
	if err := h.store.Delete(ctx, m.ID); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
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
func (h *MemoryHandler) ListSession(c *gin.Context) error {
	ctx := c.Request.Context()
	sess, err := gatedSession(c, h.sessions)
	if err != nil {
		return err
	}
	rows, err := h.store.ListScope(ctx, store.SessionMemoryScope(sess.Ref()))
	if err != nil {
		return err
	}
	out := make([]SessionMemoryInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionMemoryInfo{ID: r.ID, Key: r.Key, Bytes: len(r.Content), WrittenBy: r.WrittenBy, UpdatedAt: r.UpdatedAt})
	}
	c.JSON(http.StatusOK, out)
	return nil
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
func (h *MemoryHandler) ReadSession(c *gin.Context) error {
	ctx := c.Request.Context()
	sess, err := gatedSession(c, h.sessions)
	if err != nil {
		return err
	}
	m, err := h.store.GetByKey(ctx, store.SessionMemoryScope(sess.Ref()), strings.TrimPrefix(c.Param("key"), "/"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, m)
	return nil
}

// writable checks the caller may write req's scope and builds the row (a session
// memory under the session's current generation).
func (h *MemoryHandler) writable(c *gin.Context, req memoryReq) (*store.Memory, error) {
	ctx := c.Request.Context()
	u, ok := server.CurrentUser(c)
	if !ok {
		return nil, errNotFound
	}
	policy := store.MemoryPolicies[req.ScopeKind]
	m := &store.Memory{
		ScopeKind: req.ScopeKind, ScopeID: req.ScopeID, Key: req.Key,
		Content: req.Content, Metadata: req.Metadata,
		WrittenBy: store.MemoryWrittenByUser, OwnerID: u.ID,
	}
	switch policy.Write {
	case store.MemoryWriteAdmin:
		if u.Role != store.RoleAdmin {
			return nil, forbidden("only an admin writes global memory")
		}
	case store.MemoryWriteAgentEditor:
		ac, err := h.agents.Get(ctx, req.ScopeID)
		if err != nil {
			return nil, err
		}
		if err := editableRow(c, ac.Scope, ac.OwnerID); err != nil {
			return nil, err
		}
	case store.MemoryWriteSessionOwner:
		sess, err := h.sessions.Get(ctx, req.ScopeID)
		if err != nil || sess.OwnerID != u.ID {
			return nil, errNotFound
		}
		m.Gen = sess.Gen
	}
	return m, nil
}

// orphaned reports an agent memory whose agent is gone: an admin's to
// delete, nobody's to edit.
func (h *MemoryHandler) orphaned(ctx context.Context, m *store.Memory) bool {
	if m.ScopeKind != store.MemoryScopeAgent {
		return false
	}
	_, err := h.agents.Get(ctx, m.ScopeID)
	return errors.Is(err, store.ErrNotFound)
}

// readable checks the caller may read m; a refusal is a 404, as for any
// row the caller cannot see.
func (h *MemoryHandler) readable(c *gin.Context, m *store.Memory) error {
	ctx := c.Request.Context()
	u, ok := server.CurrentUser(c)
	if !ok {
		return errNotFound
	}
	switch m.ScopeKind {
	case store.MemoryScopeAgent:
		ac, err := h.agents.Get(ctx, m.ScopeID)
		if err != nil {
			return err
		}
		return visibleRow(c, ac.Scope, ac.OwnerID)
	case store.MemoryScopeSession:
		sess, err := h.sessions.Get(ctx, m.ScopeID)
		if err != nil || sess.OwnerID != u.ID {
			return errNotFound
		}
	}
	return nil
}

// memoryStoreError maps a policy refusal to 400 and an edit-rule refusal to 403.
func memoryStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrMemoryTooLarge), errors.Is(err, store.ErrMemoryLimit), errors.Is(err, store.ErrMemoryScope):
		return badRequest(err.Error())
	case errors.Is(err, store.ErrMemoryForbidden):
		return forbidden(err.Error())
	default:
		return err
	}
}
