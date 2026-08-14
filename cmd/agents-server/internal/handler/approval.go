package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ApprovalHandler serves the REST surface for human-in-the-loop tool
// approvals: listing what a session is waiting on, and approving/rejecting a
// pending tool call. Decisions resume the run through the shared runner hub,
// so the resulting events stream over GET /runs/{id}/events or the WebSocket.
type ApprovalHandler struct {
	store  *store.PendingApprovalStore
	runner *bridge.Runner
}

// NewApprovalHandler returns a handler backed by the pending-approval store
// and the runner. The session listing also surfaces approvals paused inside
// the session's background tasks (a join in the approval store).
func NewApprovalHandler(s *store.PendingApprovalStore, runner *bridge.Runner) *ApprovalHandler {
	return &ApprovalHandler{store: s, runner: runner}
}

// SessionApproval is a pending approval enriched with the background work it
// belongs to — a task or a workflow execution. Both empty for the session's own
// foreground run.
type SessionApproval struct {
	store.PendingApproval
	TaskID        string `json:"task_id,omitempty"`
	TaskLabel     string `json:"task_label,omitempty"`
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
	WorkflowName  string `json:"workflow_name,omitempty"`
}

// ListBySession responds with the pending approvals for the session
// identified by the id path parameter.
//
//	@Summary		List pending approvals
//	@Description	Tool calls the session is paused on, awaiting a human decision. Survives restarts.
//	@Tags			approvals
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		200	{array}		SessionApproval
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/approvals [get]
func (h *ApprovalHandler) ListBySession(c *gin.Context) {
	ctx := c.Request.Context()
	items, err := h.store.ListBySession(ctx, c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	out := make([]SessionApproval, 0, len(items))
	for _, it := range items {
		out = append(out, SessionApproval{PendingApproval: it})
	}
	// Approvals paused inside this session's background tasks surface here too,
	// tagged with their task, so the chat UI is the one approval surface.
	// One join, not a query per task.
	taskItems, err := h.store.ListByParentTasks(ctx, c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	for _, it := range taskItems {
		out = append(out, SessionApproval{PendingApproval: it.PendingApproval, TaskID: it.TaskID, TaskLabel: it.TaskLabel})
	}
	// And the ones paused inside a workflow's own session, for the same reason:
	// the chat is the ONE approval surface, and a step waiting on a decision in
	// a session nobody can open waits forever.
	wfItems, err := h.store.ListByParentWorkflows(ctx, c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	for _, it := range wfItems {
		out = append(out, SessionApproval{
			PendingApproval: it.PendingApproval,
			WorkflowRunID:   it.WorkflowRunID,
			WorkflowName:    it.WorkflowName,
		})
	}
	c.JSON(http.StatusOK, out)
}

type rejectReq struct {
	Reason string `json:"reason"`
}

type approvalResultResp struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

// Approve approves the pending tool call identified by the tool_call_id path
// parameter and resumes its run.
//
//	@Summary		Approve tool call
//	@Description	Approves a pending tool call and resumes the run under its original run id; stream it via GET /runs/{id}/events (existing cursors stay valid). For exec_command, the optional body scope extends the approval: "once" (default), "same" (trust this exact command for the session), or "all" (trust every command).
//	@Tags			approvals
//	@Accept			json
//	@Produce		json
//	@Param			tool_call_id	path		string		true	"Tool call ID"
//	@Param			body			body		approveReq	false	"Optional approval scope"
//	@Success		202				{object}	approvalResultResp
//	@Failure		404				{object}	ErrorResponse
//	@Failure		409				{object}	ErrorResponse	"session already has an active run"
//	@Failure		500				{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/approvals/{tool_call_id}/approve [post]
func (h *ApprovalHandler) Approve(c *gin.Context) {
	var req approveReq
	_ = c.ShouldBindJSON(&req) // body optional; absent → once
	h.resolve(c, true, req.toScope(), "")
}

// Reject rejects the pending tool call identified by the tool_call_id path
// parameter (with an optional reason) and resumes its run.
//
//	@Summary		Reject tool call
//	@Description	Rejects a pending tool call and resumes the run under its original run id so the model can react.
//	@Tags			approvals
//	@Accept			json
//	@Produce		json
//	@Param			tool_call_id	path		string		true	"Tool call ID"
//	@Param			body			body		rejectReq	false	"Optional rejection reason"
//	@Success		202				{object}	approvalResultResp
//	@Failure		404				{object}	ErrorResponse
//	@Failure		409				{object}	ErrorResponse	"session already has an active run"
//	@Failure		500				{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/approvals/{tool_call_id}/reject [post]
func (h *ApprovalHandler) Reject(c *gin.Context) {
	var req rejectReq
	_ = c.ShouldBindJSON(&req) // body is optional
	h.resolve(c, false, bridge.ApprovalOnce, req.Reason)
}

// approveReq is the optional body of an approve request. Scope controls how far
// the decision extends for exec_command: "once" (default), "same" (trust this
// exact command for the session), or "all" (trust every command).
type approveReq struct {
	Scope string `json:"scope"`
}

func (r approveReq) toScope() bridge.ApprovalScope {
	return bridge.ParseApprovalScope(r.Scope)
}

func (h *ApprovalHandler) resolve(c *gin.Context, approve bool, scope bridge.ApprovalScope, reason string) {
	toolCallID := c.Param("tool_call_id")
	runID, _, err := h.runner.ResolveApproval(c.Request.Context(), toolCallID, approve, scope, reason, nil)
	if err != nil {
		h.resolveError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, approvalResultResp{RunID: runID, Status: string(bridge.RunRunning)})
}

func (h *ApprovalHandler) resolveError(c *gin.Context, err error) {
	// Draining: retryable, not an internal failure.
	if down, ok := errors.AsType[bridge.ErrShuttingDown](err); ok {
		unavailable(c, down.Error())
		return
	}
	if busy, ok := errors.AsType[bridge.ErrSessionBusy](err); ok {
		conflict(c, "session already has an active run: "+busy.RunID)
		return
	}
	// Unresumable-by-version: a clear 409 with the reason, not a masked 500.
	// The stale record was already discarded, so the run is gone.
	if stale, ok := errors.AsType[*bridge.StaleApprovalStateError](err); ok {
		conflict(c, stale.Error())
		return
	}
	// The paused run reached a terminal state (a concurrent stop won) and
	// cannot be continued — a state conflict, not a server fault.
	if notResumable, ok := errors.AsType[bridge.ErrRunNotResumable](err); ok {
		conflict(c, notResumable.Error())
		return
	}
	// The task was stopped/reaped before the decision landed — 409, not 500.
	if void, ok := errors.AsType[*bridge.ApprovalVoidError](err); ok {
		conflict(c, void.Error())
		return
	}
	// The task was retried past the paused run — the approval belongs to a
	// finished attempt and was discarded. 409, not 500.
	if staleAttempt, ok := errors.AsType[*bridge.StaleApprovalAttemptError](err); ok {
		conflict(c, staleAttempt.Error())
		return
	}
	// The paused run had not finished settling; the row is preserved for a
	// retry, so it is a transient conflict.
	if notReady, ok := errors.AsType[*bridge.ApprovalNotReadyError](err); ok {
		conflict(c, notReady.Error())
		return
	}
	if deleting, ok := errors.AsType[bridge.ErrSessionDeleting](err); ok {
		conflict(c, deleting.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		notFound(c)
		return
	}
	internalError(c, err)
}
