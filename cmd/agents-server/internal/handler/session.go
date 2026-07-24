package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// RunStopper stops a session's live run and its background tasks; implemented
// by the bridge runner. Deletion must stop execution before removing data.
type RunStopper interface {
	StopSessionTree(sessionID string)
}

// SessionHandler serves CRUD endpoints for chat sessions and their messages.
type SessionHandler struct {
	sessions *store.SessionStore
	messages *store.MessageStore
	traces   *store.TraceStore
	agents   *store.AgentConfigStore
	stopper  RunStopper
}

// NewSessionHandler returns a handler backed by the session, message, trace,
// and agent-config stores.
func NewSessionHandler(sessions *store.SessionStore, messages *store.MessageStore, traces *store.TraceStore, agents *store.AgentConfigStore) *SessionHandler {
	return &SessionHandler{sessions: sessions, messages: messages, traces: traces, agents: agents}
}

// WithRunStopper wires the runner so deletes stop the session tree first.
func (h *SessionHandler) WithRunStopper(s RunStopper) *SessionHandler {
	h.stopper = s
	return h
}

// List responds with all sessions.
//
//	@Summary	List sessions
//	@Tags		sessions
//	@Produce	json
//	@Success	200	{array}		store.Session
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions [get]
func (h *SessionHandler) List(c *gin.Context) {
	sessions, err := h.sessions.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, sessions)
}

// sessionCreateReq is the request body for Create.
type sessionCreateReq struct {
	Name string `json:"name"`
	// AgentConfigID optionally binds the session to an agent up front.
	AgentConfigID string `json:"agent_config_id"`
}

// Create persists a new session, defaulting its name when omitted.
//
//	@Summary	Create session
//	@Tags		sessions
//	@Accept		json
//	@Produce	json
//	@Param		session	body		sessionCreateReq	false	"Session; name defaults to \"New	Chat\", agent_config_id optionally binds an agent"
//	@Success	201		{object}	store.Session
//	@Failure	400		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions [post]
func (h *SessionHandler) Create(c *gin.Context) {
	var req sessionCreateReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		badRequest(c, err.Error())
		return
	}
	if req.Name == "" {
		req.Name = "New Chat"
	}
	ctx := c.Request.Context()
	if req.AgentConfigID != "" {
		if _, err := h.agents.Get(ctx, req.AgentConfigID); err != nil {
			badRequest(c, "agent_config_id does not reference an existing agent")
			return
		}
	}
	sess := &store.Session{
		ID:            store.NewID(),
		Name:          req.Name,
		AgentConfigID: req.AgentConfigID,
	}
	if err := h.sessions.Create(ctx, sess); err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, sess)
}

// Get responds with the session identified by the id path parameter.
//
//	@Summary	Get session
//	@Tags		sessions
//	@Produce	json
//	@Param		id	path		string	true	"Session ID"
//	@Success	200	{object}	store.Session
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions/{id} [get]
func (h *SessionHandler) Get(c *gin.Context) {
	sess, err := h.sessions.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sess)
}

// sessionPatchReq is the request body for Patch; absent fields are unchanged.
type sessionPatchReq struct {
	Name   *string `json:"name"`
	Pinned *bool   `json:"pinned"`
}

// Patch applies a partial update (rename and/or pin) to the session
// identified by the id path parameter and responds with the updated session.
//
//	@Summary		Update session (partial)
//	@Description	Applies a partial update; absent fields are unchanged.
//	@Tags			sessions
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"Session ID"
//	@Param			session	body		sessionPatchReq	true	"Fields to change"
//	@Success		200		{object}	store.Session
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id} [patch]
func (h *SessionHandler) Patch(c *gin.Context) {
	var req sessionPatchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.Name != nil && *req.Name == "" {
		badRequest(c, "name cannot be empty")
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	if err := h.sessions.UpdateFields(ctx, id, req.Name, req.Pinned); err != nil {
		storeError(c, err)
		return
	}
	sess, err := h.sessions.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sess)
}

// Delete removes the session identified by the id path parameter together
// with its messages and traces (one transaction in the store).
//
//	@Summary	Delete session
//	@Tags		sessions
//	@Param		id	path	string	true	"Session ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions/{id} [delete]
func (h *SessionHandler) Delete(c *gin.Context) {
	// Stop the session's live run and all its background tasks (bounded wait)
	// BEFORE the cascade: a task still executing would keep writing messages
	// and traces into rows this delete is about to remove.
	if h.stopper != nil {
		h.stopper.StopSessionTree(c.Param("id"))
	}
	if err := h.sessions.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Fork creates a new session by copying messages from the source session up
// to (and including) a given message ID. When message_id is omitted (or 0),
// all messages are copied.
//
//	@Summary		Fork session
//	@Description	Copies messages (and their traces) into a new session. message_id bounds the copy; omit it to copy everything. exclusive=true excludes the boundary message itself.
//	@Tags			sessions
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string	true	"Source session ID"
//	@Param			fork	body		object	false	"{message_id?: number, exclusive?: bool, label?: string}"
//	@Success		201		{object}	store.Session
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/fork [post]
func (h *SessionHandler) Fork(c *gin.Context) {
	srcID := c.Param("id")
	ctx := c.Request.Context()

	src, err := h.sessions.Get(ctx, srcID)
	if err != nil {
		storeError(c, err)
		return
	}

	var req struct {
		MessageID *int64 `json:"message_id"`
		Exclusive bool   `json:"exclusive"`
		Label     string `json:"label"`
	}
	// An empty body means "fork everything"; anything else must parse.
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		badRequest(c, err.Error())
		return
	}

	label := req.Label
	if label == "" {
		label = "fork"
	}
	var upTo int64
	if req.MessageID != nil {
		upTo = *req.MessageID
	}

	dst := &store.Session{
		ID:            store.NewID(),
		Name:          branchName(src.Name, label),
		AgentConfigID: src.AgentConfigID,
	}
	if err := h.sessions.Create(ctx, dst); err != nil {
		internalError(c, err)
		return
	}
	runIDs, err := h.messages.ForkMessages(ctx, srcID, dst.ID, upTo, req.Exclusive)
	if err != nil {
		// The dst session was already created; a failed message copy would leave
		// it orphaned (empty, no tasks). Roll it back on a detached context so a
		// cancelled request still cleans up. Store-level atomicity across the
		// session and message copy would be stronger; this at least avoids the leak.
		_ = h.sessions.Delete(context.WithoutCancel(ctx), dst.ID)
		internalError(c, err)
		return
	}
	if h.traces != nil {
		// Traces are a best-effort copy: the fork's messages already landed, so a
		// trace-copy failure must not fail the request or orphan the new session.
		// It is logged, not swallowed, so the missing traces are diagnosable.
		if err := h.traces.ForkBySession(ctx, srcID, dst.ID, runIDs); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).
				Str("src_session", srcID).
				Str("dst_session", dst.ID).
				Msg("fork: copying traces to the new session failed; session forked without traces")
		}
	}
	c.JSON(http.StatusCreated, dst)
}

// Messages responds with the messages for the session identified by the id path parameter.
//
//	@Summary		List session messages
//	@Description	Without limit, returns all messages oldest-first. With limit, returns the newest `limit` messages (still oldest-first); page backwards by passing the smallest received id as before_id.
//	@Tags			sessions
//	@Produce		json
//	@Param			id			path		string	true	"Session ID"
//	@Param			limit		query		int		false	"Max messages to return; 0 or absent returns all"
//	@Param			before_id	query		int		false	"Only messages with id < before_id (backwards cursor)"
//	@Success		200			{array}		store.Message
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/messages [get]
func (h *SessionHandler) Messages(c *gin.Context) {
	beforeID, limit := pageParams(c)
	msgs, err := h.messages.GetMessages(c.Request.Context(), c.Param("id"), beforeID, limit)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, msgs)
}

var branchSuffixRe = regexp.MustCompile(`\s*\((fork|regen)(?:\s+(\d+))?\)$`)

func branchName(name, label string) string {
	base := branchSuffixRe.ReplaceAllString(name, "")
	m := branchSuffixRe.FindStringSubmatch(name)
	n := 1
	if m != nil && m[1] == label && m[2] != "" {
		n, _ = strconv.Atoi(m[2])
	}
	if m != nil && m[1] == label {
		n++
	}
	if n <= 1 {
		return fmt.Sprintf("%s (%s)", base, label)
	}
	return fmt.Sprintf("%s (%s %d)", base, label, n)
}
