package handler

import (
	"cmp"
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

// SessionHandler serves CRUD endpoints for chat sessions and their entries.
type SessionHandler struct {
	sessions *store.SessionStore
	entries  *store.EntryStore
	traces   *store.TraceStore
	agents   *store.AgentConfigStore
	stopper  RunStopper
}

// NewSessionHandler returns a handler backed by the session, message, trace,
// and agent-config stores.
func NewSessionHandler(sessions *store.SessionStore, entries *store.EntryStore, traces *store.TraceStore, agents *store.AgentConfigStore) *SessionHandler {
	return &SessionHandler{sessions: sessions, entries: entries, traces: traces, agents: agents}
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
	req.Name = cmp.Or(req.Name, "New Chat")
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
// with its entries and traces (one transaction in the store).
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
	// BEFORE the cascade: a task still executing would keep writing entries
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

// Fork creates a new session by copying entries from the source session up
// to (and including) a given entry row ID. When message_id is omitted (or 0),
// all entries are copied.
//
//	@Summary		Fork session
//	@Description	Copies entries (and their traces) into a new session. message_id bounds the copy; omit it to copy everything. exclusive=true excludes the boundary entry itself.
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
	label = cmp.Or(label, "fork")
	var upTo int64
	if req.MessageID != nil {
		upTo = *req.MessageID
	}

	dst := &store.Session{
		ID:            store.NewID(),
		Name:          branchName(src.Name, label),
		AgentConfigID: src.AgentConfigID,
	}
	// One transaction creates the session and copies its entries, so a failure
	// (or a cancelled request) can't leave an orphaned empty session behind.
	runIDs, err := h.entries.ForkSession(ctx, dst, srcID, upTo, req.Exclusive)
	if err != nil {
		// A source deleted out from under the fork (ErrNotFound) is a 404, not a
		// 500; storeError maps it.
		storeError(c, err)
		return
	}
	if h.traces != nil {
		// Traces are a best-effort copy: the fork's entries already landed, so a
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

// Messages responds with the session entries for the id path parameter.
//
//	@Summary		List session entries
//	@Description	Without limit, returns all entries oldest-first. With limit, returns the newest `limit` entries (still oldest-first); page backwards by passing the smallest received id as before_id. Update entries are folded into their targets server-side.
//	@Tags			sessions
//	@Produce		json
//	@Param			id			path		string	true	"Session ID"
//	@Param			limit		query		int		false	"Max entries to return; 0 or absent returns all"
//	@Param			before_id	query		int		false	"Only entries with id < before_id (backwards cursor)"
//	@Success		200			{array}		store.EntryView
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/messages [get]
func (h *SessionHandler) Messages(c *gin.Context) {
	beforeID, limit := pageParams(c)
	entries, err := h.entries.GetEntries(c.Request.Context(), c.Param("id"), beforeID, limit)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, entries)
}

type branchReq struct {
	EntryID string `json:"entry_id"`
}

// Branch moves the session's active branch to an entry.
//
//	@Summary		Switch active branch
//	@Description	Moves the session's active branch to entry_id, so the next run continues from there. Appends a leaf entry rather than deleting anything — the abandoned attempt stays recorded and can be switched back to.
//	@Tags			sessions
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string		true	"Session ID"
//	@Param			branch	body		branchReq	true	"{entry_id}"
//	@Success		200		{object}	map[string]string
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/branch [post]
func (h *SessionHandler) Branch(c *gin.Context) {
	var req branchReq
	if err := c.ShouldBindJSON(&req); err != nil || req.EntryID == "" {
		badRequest(c, "entry_id is required")
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	if err := h.entries.Branch(ctx, id, req.EntryID); err != nil {
		badRequest(c, err.Error())
		return
	}
	leaf, err := h.entries.Leaf(ctx, id)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"leaf": leaf})
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
