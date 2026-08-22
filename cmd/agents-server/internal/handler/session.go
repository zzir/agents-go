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
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// RunStopper stops a session's live run and its background tasks; implemented
// by the bridge runner. Deletion must stop execution before removing data.
type RunStopper interface {
	StopSessionTree(sessionID string)
	// AbortSessionDelete clears the deleting mark when the store delete fails:
	// the cascade rolled back, so the session still exists and must not be
	// left refusing every run until restart.
	AbortSessionDelete(sessionID string)
	// ReleaseSessionBinding releases the cached sandbox instance behind a
	// deleted session's (sandbox, workdir) binding when no other session
	// references the pair — the instance (an ssh connection, a docker
	// container) would otherwise live until process exit.
	ReleaseSessionBinding(sandboxID, workDir string)
}

// MCPToolLister answers what a connected MCP server currently exposes;
// implemented by the bridge's McpManager. The Context report sizes an MCP tool
// surface by asking, because those tools live on the server rather than on the
// agent — the build never sees them.
type MCPToolLister interface {
	ListToolsFor(ctx context.Context, serverID string) (name string, tools []*agents.Tool, err error)
}

// SessionCompactor runs one forced compaction pass on a session outside any
// run; implemented by the bridge's Runner. compacted=false with a nil error
// means the guards found nothing to fold.
type SessionCompactor interface {
	CompactSession(ctx context.Context, sessionID string) (compacted bool, beforeItems, afterItems int, err error)
}

// SessionDeps is what a SessionHandler runs on. Every field is required —
// a handler missing one would serve half its routes (a delete that stops no
// runs, a Context report with no profile), and only the wiring in cmd could
// tell — so NewSessionHandler refuses a nil.
type SessionDeps struct {
	Sessions *store.SessionStore
	Entries  *store.EntryStore
	Traces   *store.TraceStore
	Agents   *store.AgentConfigStore
	// Profiles, MCP and MCPServers are what the Context report needs beyond
	// the session's own entries: the per-session build snapshot, a way to ask
	// a connected MCP server what it currently exposes, and the server store —
	// which is what resolves a NAME for a disconnected or disabled server (the
	// manager never holds those, and their bucket would otherwise be labelled
	// by raw id).
	Profiles   *store.ContextProfileStore
	MCP        MCPToolLister
	MCPServers *store.McpServerStore
	// Stopper stops the session tree before a delete's cascade; Compactor is
	// the manual compaction pass. Both the bridge Runner.
	Stopper   RunStopper
	Compactor SessionCompactor
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
	stopper    RunStopper
	compactor  SessionCompactor
}

// NewSessionHandler returns a handler over d. It panics on a missing
// dependency: that is a wiring error, not a runtime condition.
func NewSessionHandler(d SessionDeps) *SessionHandler {
	switch {
	case d.Sessions == nil, d.Entries == nil, d.Traces == nil, d.Agents == nil,
		d.Profiles == nil, d.MCPServers == nil:
		panic("handler: SessionDeps has a nil store")
	case d.MCP == nil, d.Stopper == nil, d.Compactor == nil:
		panic("handler: SessionDeps has a nil MCP lister, stopper or compactor")
	}
	return &SessionHandler{
		sessions: d.Sessions, entries: d.Entries, traces: d.Traces, agents: d.Agents,
		profiles: d.Profiles, mcp: d.MCP, mcpServers: d.MCPServers,
		stopper: d.Stopper, compactor: d.Compactor,
	}
}

// List responds with the caller's sessions. `?all=true` is the admin's
// management view — every owner's sessions, existence and recency only (the
// same rows; content stays behind the owner checks of the per-session routes).
//
//	@Summary	List sessions
//	@Tags		sessions
//	@Produce	json
//	@Param		all	query		bool	false	"Every owner's sessions (admin only)"
//	@Success	200	{array}		store.Session
//	@Failure	403	{object}	ErrorResponse	"all=true by a member"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions [get]
func (h *SessionHandler) List(c *gin.Context) {
	u, _ := server.CurrentUser(c)
	owner := u.ID
	if c.Query("all") == "true" {
		if !requireAdmin(c) {
			return
		}
		owner = ""
	}
	sessions, err := h.sessions.List(c.Request.Context(), owner)
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
	req.Name = cmp.Or(req.Name, store.DefaultSessionName)
	ctx := c.Request.Context()
	if req.AgentConfigID != "" {
		if _, err := h.agents.Get(ctx, req.AgentConfigID); err != nil {
			badRequest(c, "agent_config_id does not reference an existing agent")
			return
		}
	}
	u, _ := server.CurrentUser(c)
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
	// `planning` rides on the row now (materialized), so the response carries it
	// with no extra read — and the list gets it for free too.
	c.JSON(http.StatusOK, sess)
}

// sessionPatchReq is the request body for Patch; absent fields are unchanged.
// The sandbox binding is deliberately NOT patchable: (sandbox_id, work_dir) is
// fixed by the first sandbox-carrying run and never rewritten — switching
// projects means starting (or forking into) another session.
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
	// The binding, read before the cascade erases it: releasing the cached
	// sandbox instance afterwards needs to know which pair this session held.
	var boundSandbox, boundWorkDir string
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
	boundSandbox, boundWorkDir = sess.SandboxID, sess.WorkDir
	// Stop the session's live run and all its background tasks (bounded wait)
	// BEFORE the cascade: a task still executing would keep writing entries
	// and traces into rows this delete is about to remove.
	h.stopper.StopSessionTree(id)
	if err := h.sessions.Delete(c.Request.Context(), id); err != nil {
		h.stopper.AbortSessionDelete(id)
		storeError(c, err)
		return
	}
	// After the cascade: the reference count the release consults no longer
	// includes this session (or its cascade-deleted task children).
	if boundSandbox != "" {
		h.stopper.ReleaseSessionBinding(boundSandbox, boundWorkDir)
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
		OwnerID:       src.OwnerID,
		Name:          branchName(src.Name, label),
		AgentConfigID: src.AgentConfigID,
		// The sandbox binding is copied, not re-bound: a fork continues the
		// same conversation over the same file system context.
		SandboxID: src.SandboxID,
		WorkDir:   src.WorkDir,
		// The plan phase carries over: a fork of a session mid-planning inherits
		// the planning it forked in (what the entry-marker approach gave for
		// free before the phase was materialized).
		Planning: src.Planning,
	}
	// One transaction creates the session and copies its entries, so a failure
	// (or a cancelled request) can't leave an orphaned empty session behind.
	srcRef, err := h.entries.RefFor(ctx, srcID)
	if err != nil {
		storeError(c, err)
		return
	}
	runIDs, err := h.entries.ForkSession(ctx, dst, srcRef, upTo, req.Exclusive)
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
			logging.Ctx(ctx).Warn("fork: copying traces to the new session failed; session forked without traces", "error", err, "src_session", srcID, "dst_session", dst.ID)
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
	ctx := c.Request.Context()
	ref, err := h.entries.RefFor(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	entries, err := h.entries.GetEntries(ctx, ref, beforeID, limit)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, entries)
}

// Runs responds with the session's runs and what each was asked.
//
//	@Summary		List session runs
//	@Description	Every run that left entries on the session, oldest first, each with the user text it started from (`question` — its own message, or for a regenerate the message it answered again) and whether its entries are on the active branch (`on_path`). What the trace panel labels a run by when its message lies outside the page of history it has loaded.
//	@Tags			sessions
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		200	{array}		store.RunQuestion
//	@Failure		404	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/runs [get]
func (h *SessionHandler) Runs(c *gin.Context) {
	ctx := c.Request.Context()
	ref, err := h.entries.RefFor(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	runs, err := h.entries.RunQuestions(ctx, ref)
	if err != nil {
		internalError(c, err)
		return
	}
	if runs == nil {
		runs = []store.RunQuestion{}
	}
	c.JSON(http.StatusOK, runs)
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
	sess, err := h.sessions.Get(ctx, c.Param("id"))
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
	// The window and the compaction threshold are the agent's, not the
	// session's. A session with no agent bound yet (or one whose config was
	// deleted) still reports its own usage — just without a denominator.
	if sess.AgentConfigID != "" {
		if ac, err := h.agents.Get(ctx, sess.AgentConfigID); err == nil {
			rep.Model = ac.Model
			rep.ContextWindow = ac.ContextWindow
			rep.CompactionEnabled = ac.Compaction.Enabled
			if ac.Compaction.Enabled {
				// Same fallback rule as NewCompactionAdapter (<= 0, not just
				// 0), so the threshold drawn is the threshold that fires.
				rep.CompactionThreshold = ac.Compaction.Threshold
				if rep.CompactionThreshold <= 0 {
					rep.CompactionThreshold = store.DefaultCompactionThresholdTokens
				}
			}
		}
	}
	// What the last run put in front of the conversation. Absent until a run
	// has built the agent once — nothing else knows what a build assembled.
	if prof, err := h.profiles.Get(ctx, sess.ID); err != nil {
		logging.Ctx(ctx).Warn("context report: prompt profile unreadable", "error", err)
	} else if prof != nil {
		prof.Tools = append(prof.Tools, h.mcpBuckets(ctx, prof.MCPServerIDs)...)
		rep.Prompt = prof
	}
	c.JSON(http.StatusOK, rep)
}

// CompactResponse reports what a manual compaction pass did. Compacted false
// means the guards found nothing to fold — the kept window already covers the
// history.
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

// contextMCPTimeout bounds the tools/list calls one context report makes. The
// panel is a diagnostic: a slow server costs it that server's row, not the
// report.
const contextMCPTimeout = 2 * time.Second

// mcpBuckets sizes each connected MCP server's tool surface. A server that is
// gone or slow is reported UNAVAILABLE rather than zero — "0 tokens of MCP" and
// "we could not ask" are opposite answers for someone hunting a full window.
//
// The servers are asked CONCURRENTLY: sequentially, three slow ones would spend
// the whole budget one after another and the panel would wait for the sum.
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
				// The manager only knows CONNECTED servers; a disconnected or
				// disabled one still has its configured name in the store, and
				// a bucket labelled by raw id names nothing a person set.
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
	ref, err := h.entries.RefFor(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if err := h.entries.Branch(ctx, ref, req.EntryID); err != nil {
		badRequest(c, err.Error())
		return
	}
	leaf, err := h.entries.Leaf(ctx, ref)
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
