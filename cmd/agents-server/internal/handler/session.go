package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// SessionHandler serves CRUD endpoints for chat sessions and their messages.
type SessionHandler struct {
	sessions *store.SessionStore
	messages *store.MessageStore
	traces   *store.TraceStore
}

// NewSessionHandler returns a handler backed by the session, message, and trace stores.
func NewSessionHandler(sessions *store.SessionStore, messages *store.MessageStore, traces *store.TraceStore) *SessionHandler {
	return &SessionHandler{sessions: sessions, messages: messages, traces: traces}
}

// List responds with all sessions.
func (h *SessionHandler) List(c *gin.Context) {
	sessions, err := h.sessions.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sessions)
}

// sessionReq is the request body for both Create and Update.
type sessionReq struct {
	Name string `json:"name"`
}

// Create persists a new session, defaulting its name when omitted.
func (h *SessionHandler) Create(c *gin.Context) {
	var req sessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name == "" {
		req.Name = "New Chat"
	}
	sess := &store.Session{
		ID:   store.NewID(),
		Name: req.Name,
	}
	if err := h.sessions.Create(c.Request.Context(), sess); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, sess)
}

// Get responds with the session identified by the id path parameter.
func (h *SessionHandler) Get(c *gin.Context) {
	sess, err := h.sessions.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sess)
}

// Update renames the session identified by the id path parameter.
func (h *SessionHandler) Update(c *gin.Context) {
	var req sessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.sessions.Update(c.Request.Context(), c.Param("id"), req.Name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Delete removes the session identified by the id path parameter and its traces.
func (h *SessionHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	if err := h.sessions.Delete(ctx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.messages.DeleteBySession(ctx, id)
	if h.traces != nil {
		_ = h.traces.DeleteBySession(ctx, id)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Fork creates a new session by copying messages from the source session up to
// (and including) a given message ID. When message_id is 0 or omitted, all
// messages are copied.
func (h *SessionHandler) Fork(c *gin.Context) {
	srcID := c.Param("id")
	ctx := c.Request.Context()

	src, err := h.sessions.Get(ctx, srcID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	var req struct {
		MessageID int64  `json:"message_id"`
		Exclusive bool   `json:"exclusive"`
		Label     string `json:"label"`
	}
	_ = c.ShouldBindJSON(&req)

	label := req.Label
	if label == "" {
		label = "fork"
	}

	dst := &store.Session{
		ID:            store.NewID(),
		Name:          branchName(src.Name, label),
		AgentConfigID: src.AgentConfigID,
	}
	if err := h.sessions.Create(ctx, dst); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	runIDs, err := h.messages.ForkMessages(ctx, srcID, dst.ID, req.MessageID, req.Exclusive)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if h.traces != nil {
		_ = h.traces.ForkBySession(ctx, srcID, dst.ID, runIDs)
	}
	c.JSON(http.StatusCreated, dst)
}

// Pin toggles the pinned state of the session identified by the id path parameter.
func (h *SessionHandler) Pin(c *gin.Context) {
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.sessions.SetPinned(c.Request.Context(), c.Param("id"), req.Pinned); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Messages responds with the messages for the session identified by the id path parameter.
func (h *SessionHandler) Messages(c *gin.Context) {
	msgs, err := h.messages.GetMessages(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
