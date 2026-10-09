package handler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// RunStopper stops a session's live run and its background tasks; implemented
// by the bridge runner. Deletion must stop execution before removing data.
type RunStopper interface {
	StopSessionTree(sessionID string)
	// EndSessionDelete lifts the deleting mark once the store delete has ended,
	// committed or not.
	EndSessionDelete(sessionID string)
	// ReleaseSessionBinding releases the cached sandbox instance behind a
	// deleted session's project binding when no other session references it.
	ReleaseSessionBinding(projectID string)
	// ForgetSessionTrust drops a deleted session's exec_command trust grants.
	ForgetSessionTrust(sessionID string)
	// WithSessionTreeFenced runs fn with the session tree at rest and fenced
	// against new runs; bridge.ErrSessionBusy when a run is live on any of them.
	WithSessionTreeFenced(ctx context.Context, sessionID string, fn func() error) error
}

// MCPToolLister answers what a connected MCP server currently exposes
// (the bridge's McpManager); the Context report sizes MCP tool surfaces by asking.
type MCPToolLister interface {
	ListToolsFor(ctx context.Context, serverID string) (name string, tools []*agents.Tool, err error)
}

// SessionCompactor runs one forced compaction pass on a session outside any
// run (the bridge's Runner). compacted=false with a nil error means nothing to fold.
type SessionCompactor interface {
	CompactSession(ctx context.Context, sessionID string) (compacted bool, beforeItems, afterItems int, err error)
}

// SessionStatuser derives conversations' statuses and announces a change this
// handler made to one (the bridge's Runner).
type SessionStatuser interface {
	SessionStatuses(ctx context.Context, ownerID string, ids []string) (bridge.SessionStates, error)
	PublishSessionStatus(ctx context.Context, sessionID string)
}

// SessionDeps is what a SessionHandler runs on. Every field is required;
// NewSessionHandler refuses a nil.
type SessionDeps struct {
	Sessions *store.SessionStore
	Entries  *store.EntryStore
	Traces   *store.TraceStore
	Agents   *store.AgentConfigStore
	// Profiles, MCP and MCPServers serve the Context report: the build
	// snapshot, live tool listings, and the NAME of a disconnected server.
	Profiles   *store.ContextProfileStore
	MCP        MCPToolLister
	MCPServers *store.McpServerStore
	// Users resolves the account a session is reassigned to; Projects answers
	// whether that account owns a bound session's project (SetOwner's gate).
	Users    *store.UserStore
	Projects *store.ProjectStore
	// Stopper stops the session tree before a delete's cascade; Compactor is
	// the manual compaction pass. Both the bridge Runner.
	Stopper   RunStopper
	Compactor SessionCompactor
	// Statuses derives the status each listed session carries.
	Statuses SessionStatuser
	// Settings resolves the attachment public base URL for message views.
	Settings *settings.Reader
}

// SessionHandler serves CRUD endpoints for chat sessions and their entries.
type SessionHandler struct {
	sessions   *store.SessionStore
	entries    *store.EntryStore
	traces     *store.TraceStore
	agents     *store.AgentConfigStore
	profiles   *store.ContextProfileStore
	mcp        MCPToolLister
	mcpServers *store.McpServerStore
	users      *store.UserStore
	projects   *store.ProjectStore
	stopper    RunStopper
	compactor  SessionCompactor
	statuses   SessionStatuser
	settings   *settings.Reader
}

// NewSessionHandler returns a handler over d. It panics on a missing
// dependency: that is a wiring error, not a runtime condition.
func NewSessionHandler(d SessionDeps) *SessionHandler {
	switch {
	case d.Sessions == nil, d.Entries == nil, d.Traces == nil, d.Agents == nil,
		d.Profiles == nil, d.MCPServers == nil, d.Users == nil, d.Projects == nil:
		panic("handler: SessionDeps has a nil store")
	case d.MCP == nil, d.Stopper == nil, d.Compactor == nil, d.Statuses == nil, d.Settings == nil:
		panic("handler: SessionDeps has a nil MCP lister, stopper, compactor, statuses or settings")
	}
	return &SessionHandler{
		sessions: d.Sessions, entries: d.Entries, traces: d.Traces, agents: d.Agents,
		profiles: d.Profiles, mcp: d.MCP, mcpServers: d.MCPServers, users: d.Users,
		projects: d.Projects, stopper: d.Stopper, compactor: d.Compactor, statuses: d.Statuses,
		settings: d.Settings,
	}
}

// sessionView is a session with its derived status.
type sessionView struct {
	store.Session
	// Status is idle, running, requires_action or failed.
	Status string `json:"status"`
	// LiveRunID is the session's own executing run; a run paused for approval
	// is not live.
	LiveRunID string `json:"live_run_id,omitempty"`
	// PendingCount is how many decisions the session and its background tasks wait on.
	PendingCount int `json:"pending_count"`
	// OldestPendingAt is when the longest-waiting decision was asked for.
	OldestPendingAt *time.Time `json:"oldest_pending_at,omitempty"`
}

// sessionDetail is a session view with the decisions it waits on.
type sessionDetail struct {
	sessionView
	// Pending lists the decisions waited on, oldest first.
	Pending []bridge.PendingCall `json:"pending"`
}

func newSessionView(sess store.Session, st bridge.SessionState) sessionView {
	w := st.Wire(sess.ID)
	return sessionView{
		Session: sess, Status: w.Status, LiveRunID: w.LiveRunID,
		PendingCount: w.PendingCount, OldestPendingAt: w.OldestPendingAt,
	}
}

// List responds with the caller's sessions, each with its derived status;
// `?all=true` is the admin's view of every owner's.
//
//	@Summary		List sessions
//	@Description	Newest first by updated_at. Pinned sessions come whole with the first page; limit counts the unpinned ones and before (a session id from the previous page) continues after it. q matches the name or the first user message, case-insensitively. Without limit the whole list is returned.
//	@Tags			sessions
//	@Produce		json
//	@Param			all		query		bool	false	"Every owner's sessions (admin only)"
//	@Param			limit	query		int		false	"Unpinned sessions per page (0 = all)"
//	@Param			before	query		string	false	"Continue after this session id (the last of the previous page)"
//	@Param			q		query		string	false	"Match the name or the first user message"
//	@Success		200		{array}		sessionView
//	@Failure		400		{object}	ErrorResponse	"limit is not a non-negative integer"
//	@Failure		403		{object}	ErrorResponse	"all=true by a member"
//	@Failure		404		{object}	ErrorResponse	"before names no session of the caller"
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions [get]
func (h *SessionHandler) List(c *gin.Context) {
	u, _ := server.CurrentUser(c)
	owner := u.ID
	if c.Query("all") == "true" {
		if !requireAdmin(c) {
			return
		}
		owner = store.EveryOwner
	}
	limit, ok := queryInt(c, "limit")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	sessions, err := h.sessions.ListPage(ctx, owner, store.SessionPage{Limit: limit, Before: c.Query("before"), Query: strings.TrimSpace(c.Query("q"))})
	if err != nil {
		storeError(c, err) // an unknown cursor → 404
		return
	}
	ids := make([]string, 0, len(sessions))
	for i := range sessions {
		ids = append(ids, sessions[i].ID)
	}
	states, err := h.statuses.SessionStatuses(ctx, owner, ids)
	if err != nil {
		internalError(c, err)
		return
	}
	views := make([]sessionView, 0, len(sessions)) // an empty list, never JSON null
	for _, sess := range sessions {
		views = append(views, newSessionView(sess, states.Of(sess.ID)))
	}
	c.JSON(http.StatusOK, views)
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
//	@Failure	403		{object}	ErrorResponse	"an admin binding a member's private agent"
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions [post]
func (h *SessionHandler) Create(c *gin.Context) {
	var req sessionCreateReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		badRequest(c, err.Error())
		return
	}
	req.Name = cmp.Or(req.Name, store.DefaultSessionName)
	if !nameFits(c, req.Name) {
		return
	}
	ctx := c.Request.Context()
	u, _ := server.CurrentUser(c)
	if req.AgentConfigID != "" {
		// A foreign private agent reads as absent to a member, 403 to an admin
		// — decisions §5.29.
		ac, err := h.agents.Get(ctx, req.AgentConfigID)
		if err != nil && !errors.Is(err, store.ErrNotFound) && !store.IsMalformedID(err) {
			storeError(c, err)
			return
		}
		if err == nil && !runnableRow(c, ac.Scope, ac.OwnerID) {
			return
		}
		if err != nil || !store.Visible(ac.Scope, ac.OwnerID, u.ID, false) {
			badRequest(c, "agent_config_id does not reference an existing agent")
			return
		}
	}
	sess := &store.Session{
		ID:            store.NewID(),
		OwnerID:       u.ID,
		Name:          req.Name,
		AgentConfigID: req.AgentConfigID,
	}
	if err := h.sessions.Create(ctx, sess); err != nil {
		internalError(c, err)
		return
	}
	created(c, sess.ID, sess)
}

// Get responds with the session identified by the id path parameter, its
// derived status and the decisions it waits on.
//
//	@Summary	Get session
//	@Tags		sessions
//	@Produce	json
//	@Param		id	path		string	true	"Session ID"
//	@Success	200	{object}	sessionDetail
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions/{id} [get]
func (h *SessionHandler) Get(c *gin.Context) {
	sess, err := gatedSession(c, h.sessions)
	if err != nil {
		storeError(c, err)
		return
	}
	states, err := h.statuses.SessionStatuses(c.Request.Context(), store.EveryOwner, []string{sess.ID})
	if err != nil {
		internalError(c, err)
		return
	}
	st := states.Of(sess.ID)
	pending := st.Pending
	if pending == nil {
		pending = []bridge.PendingCall{}
	}
	// planning is a column of the row: no extra read.
	c.JSON(http.StatusOK, sessionDetail{sessionView: newSessionView(*sess, st), Pending: pending})
}

// sessionPatchReq is the request body for Patch; absent fields are unchanged.
// The project binding is NOT patchable (decisions §5.28).
type sessionPatchReq struct {
	Name   *string `json:"name"`
	Pinned *bool   `json:"pinned"`
}

// Patch applies a partial update (rename and/or pin) to the session identified
// by the id path parameter.
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
	if req.Name != nil && !nameFits(c, *req.Name) {
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	if req.Name != nil || req.Pinned != nil {
		if err := h.sessions.UpdateFields(ctx, id, req.Name, req.Pinned); err != nil {
			storeError(c, err)
			return
		}
	}
	sess, err := h.sessions.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sess)
}

// SetOwner reassigns the session tree to another account (admin); refused while
// a run or task is live in it (invariant 82) or to a non-owner of its project
// (invariant 52).
//
//	@Summary	Reassign session owner (admin)
//	@Tags		sessions
//	@Accept		json
//	@Param		id		path	string			true	"Session ID"
//	@Param		body	body	SetOwnerRequest	true	"The new owner"
//	@Success	204		"reassigned"
//	@Failure	400		{object}	ErrorResponse	"malformed body, or no such user"
//	@Failure	403		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	409		{object}	ErrorResponse	"a run or a background task is live on the session, or it is bound to a project the new owner does not own"
//	@Security	BearerAuth
//	@Router		/sessions/{id}/owner [put]
func (h *SessionHandler) SetOwner(c *gin.Context) {
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		badRequest(c, "user_id is required")
		return
	}
	id := c.Param("id")
	if _, err := h.users.ByID(c.Request.Context(), req.UserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			badRequest(c, "no such user")
			return
		}
		storeError(c, err)
		return
	}
	sess, err := h.sessions.Get(c.Request.Context(), id)
	if err != nil {
		storeError(c, err)
		return
	}
	// A project-bound session transfers only to the project's owner — invariant 52.
	if sess.ProjectID != "" {
		proj, perr := h.projects.Get(c.Request.Context(), sess.ProjectID)
		if perr != nil && !errors.Is(perr, store.ErrNotFound) {
			storeError(c, perr)
			return
		}
		if perr != nil || proj.OwnerID != req.UserID {
			conflict(c, "the session is bound to a project the new owner does not own")
			return
		}
	}
	// At rest and hub-fenced for the write — invariant 82.
	err = h.stopper.WithSessionTreeFenced(c.Request.Context(), id, func() error {
		return h.sessions.SetOwner(c.Request.Context(), id, req.UserID)
	})
	if err != nil {
		fencedError(c, err)
		return
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
}

// fencedError maps a fenced write's failure: a live run or task is 409, a
// badRequestError 400, the rest as storeError.
func fencedError(c *gin.Context, err error) {
	if _, busy := errors.AsType[bridge.ErrSessionBusy](err); busy {
		conflict(c, "a run or a background task is live on this session; stop it first")
		return
	}
	if bad, ok := errors.AsType[badRequestError](err); ok {
		badRequest(c, bad.Error())
		return
	}
	storeError(c, err)
}

// SetOwnerRequest is the body of PUT /sessions/:id/owner.
type SetOwnerRequest struct {
	UserID string `json:"user_id"`
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
	id := c.Param("id")
	// Read the binding before the cascade erases it; the release below needs it.
	var boundProject string
	sess, err := h.sessions.Get(c.Request.Context(), id)
	if err != nil {
		storeError(c, err)
		return
	}
	// Management, not reading: the owner or an admin.
	if u, _ := server.CurrentUser(c); !ownsSession(c, sess) && u.Role != store.RoleAdmin {
		notFound(c)
		return
	}
	boundProject = sess.ProjectID
	// Stop the live run and every task before the cascade, or one keeps
	// writing into deleted rows.
	h.stopper.StopSessionTree(id)
	err = h.sessions.Delete(c.Request.Context(), id)
	h.stopper.EndSessionDelete(id)
	if err != nil {
		storeError(c, err)
		return
	}
	// After the cascade, so the reference count no longer includes this session.
	if boundProject != "" {
		h.stopper.ReleaseSessionBinding(boundProject)
	}
	h.stopper.ForgetSessionTrust(id)
	c.Status(http.StatusNoContent)
}

// Fork creates a new session by copying the source's entries up to message_id.
//
//	@Summary		Fork session
//	@Description	Copies entries (and their traces) into a new session. message_id bounds the copy; omit it to copy everything. exclusive=true excludes the boundary entry itself.
//	@Tags			sessions
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string	true	"Source session ID"
//	@Param			fork	body		object	false	"{message_id?: string, exclusive?: bool, label?: string}"
//	@Success		201		{object}	store.Session
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/fork [post]
func (h *SessionHandler) Fork(c *gin.Context) {
	srcID := c.Param("id")
	ctx := c.Request.Context()

	src, err := gatedSession(c, h.sessions)
	if err != nil {
		storeError(c, err)
		return
	}

	var req struct {
		MessageID *string `json:"message_id"`
		Exclusive bool    `json:"exclusive"`
		Label     string  `json:"label"`
	}
	// An empty body means "fork everything"; anything else must parse.
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		badRequest(c, err.Error())
		return
	}

	label := req.Label
	label = cmp.Or(label, "fork")
	var upTo string
	if req.MessageID != nil {
		upTo = *req.MessageID
	}

	dst := &store.Session{
		ID:            store.NewID(),
		OwnerID:       src.OwnerID,
		Name:          branchName(src.Name, label),
		AgentConfigID: src.AgentConfigID,
		// The project binding is copied: a fork continues over the same files.
		ProjectID: src.ProjectID,
		// The plan phase carries over — invariant 33.
		Planning: src.Planning,
	}
	// One transaction creates the session and copies its entries.
	srcRef, err := h.entries.RefFor(ctx, srcID)
	if err != nil {
		storeError(c, err)
		return
	}
	runIDs, err := h.entries.ForkSession(ctx, dst, srcRef, upTo, req.Exclusive)
	if err != nil {
		// A source deleted meanwhile is a 404 (storeError).
		storeError(c, err)
		return
	}
	// Traces are a best-effort copy: a failure is logged, the fork stands.
	if err := h.traces.ForkBySession(ctx, srcID, dst.ID, runIDs); err != nil {
		logging.Ctx(ctx).Warn("fork: copying traces to the new session failed; session forked without traces", "error", err, "src_session", srcID, "dst_session", dst.ID)
	}
	created(c, dst.ID, dst)
}

// Messages responds with the session entries for the id path parameter.
//
//	@Summary		List session entries
//	@Description	Returns every entry of the session, oldest first. Update entries are folded into their targets server-side.
//	@Tags			sessions
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		200	{array}		store.EntryView
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/messages [get]
func (h *SessionHandler) Messages(c *gin.Context) {
	ctx := c.Request.Context()
	ref, err := h.entries.RefFor(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	entries, err := h.entries.GetEntries(ctx, ref)
	if err != nil {
		internalError(c, err)
		return
	}
	// The URL is a deployment fact (the current public base), filled here.
	if base := h.settings.S3Config(ctx).PublicBaseURL; base != "" {
		for i := range entries {
			fillAttachmentURLs(base, entries[i].Attachments)
		}
	}
	c.JSON(http.StatusOK, entries)
}

// Context responds with the session's context-window report.
//
//	@Summary		Session context usage
//	@Description	What the session's active branch occupies of its model's context window: the last call's provider token counts, the session totals, the per-call growth curve, the compaction estimate, and the heaviest entries still in context. Window figures are the provider's; compaction figures and item sizes are character estimates — the two are not the same ruler.
//	@Tags			sessions
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		200	{object}	store.ContextReport
//	@Failure		404	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/context [get]
func (h *SessionHandler) Context(c *gin.Context) {
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
	rep, err := h.entries.ContextReport(ctx, ref)
	if err != nil {
		internalError(c, err)
		return
	}
	// The window and the compaction threshold are the agent's; a session with
	// no agent bound still reports its usage, without a denominator.
	if sess.AgentConfigID != "" {
		if ac, err := h.agents.Get(ctx, sess.AgentConfigID); err == nil {
			rep.Model = ac.Model
			rep.ContextWindow = ac.ContextWindow
			rep.CompactionEnabled = ac.Compaction.Enabled
			rep.CompactionMode = ac.Compaction.Mode
			if ac.Compaction.Enabled {
				// Same fallback rule as NewCompactionAdapter (<= 0), so the
				// drawn threshold is the one that fires.
				rep.CompactionThreshold = ac.Compaction.Threshold
				if rep.CompactionThreshold <= 0 {
					rep.CompactionThreshold = store.DefaultCompactionThresholdTokens
				}
			}
		}
	}
	// What the last run put in front of the conversation; absent until a run
	// has built the agent.
	if prof, err := h.profiles.Get(ctx, sess.ID); err != nil {
		logging.Ctx(ctx).Warn("context report: prompt profile unreadable", "error", err)
	} else if prof != nil {
		prof.Tools = append(prof.Tools, h.mcpBuckets(ctx, prof.MCPServerIDs)...)
		rep.Prompt = prof
	}
	c.JSON(http.StatusOK, rep)
}

// CompactResponse reports what a manual compaction pass did. Compacted false
// means the guards found nothing to fold.
type CompactResponse struct {
	Compacted   bool `json:"compacted"`
	BeforeItems int  `json:"before_items,omitempty"`
	AfterItems  int  `json:"after_items,omitempty"`
}

// Compact runs one forced compaction pass on the session, outside any run.
//
//	@Summary		Compact session history
//	@Description	Folds the session's active branch down to its kept window plus a summary checkpoint, regardless of the threshold — the Context panel's "Compact now". A 200 with compacted=false means there was nothing to fold (the kept window already covers the history). Fails 409 while a run is executing and 400 when the session's agent has compaction disabled or no usable provider.
//	@Tags			sessions
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		200	{object}	CompactResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"session already has an active run"
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/compact [post]
func (h *SessionHandler) Compact(c *gin.Context) {
	compacted, before, after, err := h.compactor.CompactSession(c.Request.Context(), c.Param("id"))
	if err != nil {
		if busy, ok := errors.AsType[bridge.ErrSessionBusy](err); ok {
			conflict(c, busy.Error())
			return
		}
		if errors.Is(err, bridge.ErrCompactionUnavailable) {
			badRequest(c, err.Error())
			return
		}
		storeError(c, err) // not-found → 404, anything else → 500
		return
	}
	c.JSON(http.StatusOK, CompactResponse{Compacted: compacted, BeforeItems: before, AfterItems: after})
}

// contextMCPTimeout bounds one tools/list made on a request's behalf; a slow
// server costs the report that server's row, not the report.
const contextMCPTimeout = 2 * time.Second

// mcpBuckets sizes each connected MCP server's tool surface, asking the
// servers concurrently; a server that is gone or slow reports UNAVAILABLE, not zero.
func (h *SessionHandler) mcpBuckets(ctx context.Context, ids []string) []store.ToolBucket {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, contextMCPTimeout)
	defer cancel()
	buckets := make([]store.ToolBucket, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			name, tools, err := h.mcp.ListToolsFor(ctx, id)
			if name == "" {
				// The manager only knows CONNECTED servers; a disconnected one
				// still has its configured name in the store.
				name = h.mcpServerName(ctx, id)
			}
			b := store.ToolBucket{Source: store.ToolSourceMCP + cmp.Or(name, id)}
			if err != nil {
				b.Unavailable = true
			} else {
				b.Count = len(tools)
				for _, t := range tools {
					b.Chars += store.ToolChars(t)
				}
			}
			buckets[i] = b
		})
	}
	wg.Wait()
	return buckets
}

// mcpServerName reads a server's configured name off its row; empty when the
// row is gone (a build snapshot can outlive the server it named).
func (h *SessionHandler) mcpServerName(ctx context.Context, id string) string {
	srv, err := h.mcpServers.Get(ctx, id)
	if err != nil {
		return ""
	}
	return srv.Name
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
//	@Failure		409		{object}	ErrorResponse	"a run or a background task is live on the session"
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/branch [post]
func (h *SessionHandler) Branch(c *gin.Context) {
	var req branchReq
	if err := c.ShouldBindJSON(&req); err != nil || req.EntryID == "" {
		badRequest(c, "entry_id is required")
		return
	}
	id := c.Param("id")
	ctx := c.Request.Context()
	ref, err := h.entries.RefFor(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	// A live run keeps appending to the branch it started on, so the move
	// happens with the session fenced — invariant 82.
	var leaf, previousLeaf string
	err = h.stopper.WithSessionTreeFenced(ctx, id, func() error {
		// The leaf before the switch, so the client can roll back.
		var err error
		if previousLeaf, err = h.entries.Leaf(ctx, ref); err != nil {
			return err
		}
		if err := h.entries.Branch(ctx, ref, req.EntryID); err != nil {
			return badRequestError(err.Error())
		}
		leaf, err = h.entries.Leaf(ctx, ref)
		return err
	})
	if err != nil {
		fencedError(c, err)
		return
	}
	// A pause the move left behind, or returned to, changes what the session
	// waits on.
	h.statuses.PublishSessionStatus(ctx, id)
	c.JSON(http.StatusOK, gin.H{"leaf": leaf, "previous_leaf": previousLeaf})
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
	suffix := " (" + label + ")"
	if n > 1 {
		suffix = fmt.Sprintf(" (%s %d)", label, n)
	}
	// The suffix must fit inside the cap a rename is held to.
	return store.ClipRunes(base, maxNameLen-len([]rune(suffix))) + suffix
}
