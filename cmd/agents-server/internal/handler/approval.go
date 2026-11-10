package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ApprovalHandler serves the REST surface for human-in-the-loop approvals:
// listing what waits, and approving or rejecting a pending tool call.
type ApprovalHandler struct {
	store  *store.PendingApprovalStore
	runner *bridge.Runner
}

// NewApprovalHandler returns a handler backed by the pending-approval store
// and the runner.
func NewApprovalHandler(s *store.PendingApprovalStore, runner *bridge.Runner) *ApprovalHandler {
	return &ApprovalHandler{store: s, runner: runner}
}

// SessionApproval is a pending approval enriched with the background task it
// belongs to; empty for the session's own foreground run.
type SessionApproval struct {
	store.PendingApproval
	TaskID    string `json:"task_id,omitempty"`
	TaskLabel string `json:"task_label,omitempty"`
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
func (h *ApprovalHandler) ListBySession(c *gin.Context) error {
	ctx := c.Request.Context()
	items, err := h.store.ListBySession(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	out := make([]SessionApproval, 0, len(items))
	for _, it := range items {
		out = append(out, SessionApproval{PendingApproval: it})
	}
	// Approvals paused inside the session's background tasks surface here too,
	// tagged with their task.
	taskItems, err := h.store.ListByParentTasks(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	for _, it := range taskItems {
		out = append(out, SessionApproval{PendingApproval: it.PendingApproval, TaskID: it.TaskID, TaskLabel: it.TaskLabel})
	}
	c.JSON(http.StatusOK, out)
	return nil
}

// List responds with every decision the caller's conversations wait on.
//
//	@Summary		List the caller's pending approvals
//	@Description	Every tool call and workflow step waiting on the caller, across conversations and their background tasks, oldest first. Read-only: a decision is made on the call itself.
//	@Tags			approvals
//	@Produce		json
//	@Success		200	{array}		bridge.PendingCall
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/approvals [get]
func (h *ApprovalHandler) List(c *gin.Context) error {
	u, _ := server.CurrentUser(c)
	states, err := h.runner.SessionStatuses(c.Request.Context(), u.ID, nil)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, states.Pending())
	return nil
}

type rejectReq struct {
	Reason string `json:"reason"`
}

type approvalResultResp struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

// Approve approves the pending tool call identified by the tool_call_id path parameter.
//
//	@Summary		Approve tool call
//	@Description	Approves a pending tool call and resumes the run under its original run id; stream it via GET /runs/{id}/events (existing cursors stay valid). For exec_command, the optional body scope extends the approval: "once" (default), "same" (trust this exact command for the session), or "all" (trust every command).
//	@Tags			approvals
//	@Accept			json
//	@Produce		json
//	@Param			tool_call_id	path		string		true	"Tool call ID"
//	@Param			body			body		approveReq	false	"Optional approval scope"
//	@Success		202				{object}	approvalResultResp
//	@Failure		400				{object}	ErrorResponse	"body present but not valid JSON"
//	@Failure		404				{object}	ErrorResponse
//	@Failure		409				{object}	ErrorResponse	"session already has an active run"
//	@Failure		500				{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/approvals/{tool_call_id}/approve [post]
func (h *ApprovalHandler) Approve(c *gin.Context) error {
	var req approveReq
	if err := bindOptional(c, &req); err != nil { // absent → once
		return err
	}
	return h.resolve(c, true, req.toScope(), "")
}

// Reject rejects the pending tool call identified by the tool_call_id path parameter.
//
//	@Summary		Reject tool call
//	@Description	Rejects a pending tool call and resumes the run under its original run id so the model can react.
//	@Tags			approvals
//	@Accept			json
//	@Produce		json
//	@Param			tool_call_id	path		string		true	"Tool call ID"
//	@Param			body			body		rejectReq	false	"Optional rejection reason"
//	@Success		202				{object}	approvalResultResp
//	@Failure		400				{object}	ErrorResponse	"body present but not valid JSON"
//	@Failure		404				{object}	ErrorResponse
//	@Failure		409				{object}	ErrorResponse	"session already has an active run"
//	@Failure		500				{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/approvals/{tool_call_id}/reject [post]
func (h *ApprovalHandler) Reject(c *gin.Context) error {
	var req rejectReq
	if err := bindOptional(c, &req); err != nil {
		return err
	}
	return h.resolve(c, false, bridge.ApprovalOnce, req.Reason)
}

// approveAllResp names the resumed run and how many calls were approved.
type approveAllResp struct {
	RunID string `json:"run_id"`
	// Approved counts the calls approved; the pause's other calls (a plan, a
	// workflow save, a memory proposal) stay for a decision each.
	Approved int `json:"approved"`
}

// ApproveAll approves every call of the session's own pause at once.
//
//	@Summary		Approve every pending call of the session's pause
//	@Description	Approves, once each, every tool call the session's own run is paused on — a plan, a workflow save and a memory proposal excepted, which stay for a decision each — and resumes the run once. Background tasks' pauses are not covered: they are answered on the task. 404 when the session has no pause of its own, 400 when every call of it is one confirmed one by one, 409 while the run is live.
//	@Tags			approvals
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		202	{object}	approveAllResp
//	@Failure		400	{object}	ErrorResponse	"nothing approve-all may take"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"session already has an active run"
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/approvals/approve-all [post]
func (h *ApprovalHandler) ApproveAll(c *gin.Context) error {
	server.SetAuditDetail(c, "approve-all")
	runID, n, err := h.runner.ApproveAll(c.Request.Context(), c.Param("id"), nil)
	if err != nil {
		if errors.Is(err, bridge.ErrNothingToApproveAll) {
			return badRequest(err.Error())
		}
		return h.resolveError(err)
	}
	c.JSON(http.StatusAccepted, approveAllResp{RunID: runID, Approved: n})
	return nil
}

// approveReq is the optional body of an approve request. Scope extends an
// exec_command decision: "once" (default), "same" (this exact command), "all".
type approveReq struct {
	Scope string `json:"scope"`
}

func (r approveReq) toScope() bridge.ApprovalScope {
	return bridge.ParseApprovalScope(r.Scope)
}

func (h *ApprovalHandler) resolve(c *gin.Context, approve bool, scope bridge.ApprovalScope, reason string) error {
	toolCallID := c.Param("tool_call_id")
	server.SetAuditDetail(c, auditDecision(approve, scope, pendingCall(c)))
	runID, _, err := h.runner.ResolveApproval(c.Request.Context(), toolCallID, approve, scope, reason, nil)
	if err != nil {
		return h.resolveError(err)
	}
	// A decision that resumed or started a run names it; a rejected workflow
	// step starts nothing — the execution is cancelled and no run exists.
	status := string(bridge.RunRunning)
	if runID == "" {
		status = string(bridge.RunCancelled)
	}
	c.JSON(http.StatusAccepted, approvalResultResp{RunID: runID, Status: status})
	return nil
}

func (h *ApprovalHandler) resolveError(err error) error {
	// Draining: retryable, not an internal failure.
	if down, ok := errors.AsType[bridge.ErrShuttingDown](err); ok {
		return unavailable(down.Error())
	}
	if busy, ok := errors.AsType[bridge.ErrSessionBusy](err); ok {
		return conflict("session already has an active run: " + busy.RunID)
	}
	// Unresumable by version: 409 with the reason; the stale record is already
	// discarded.
	if stale, ok := errors.AsType[*bridge.StaleApprovalStateError](err); ok {
		return conflict(stale.Error())
	}
	// The paused run reached a terminal state (a concurrent stop won): 409.
	if notResumable, ok := errors.AsType[bridge.ErrRunNotResumable](err); ok {
		return conflict(notResumable.Error())
	}
	// The task was stopped/reaped before the decision landed — 409, not 500.
	if void, ok := errors.AsType[*bridge.ApprovalVoidError](err); ok {
		return conflict(void.Error())
	}
	// The task was retried past the paused run and the approval discarded: 409.
	if staleAttempt, ok := errors.AsType[*bridge.StaleApprovalAttemptError](err); ok {
		return conflict(staleAttempt.Error())
	}
	// The paused run had not finished settling; the row is kept for a retry: 409.
	if notReady, ok := errors.AsType[*bridge.ApprovalNotReadyError](err); ok {
		return conflict(notReady.Error())
	}
	if deleting, ok := errors.AsType[bridge.ErrSessionDeleting](err); ok {
		return conflict(deleting.Error())
	}
	return err
}

// auditDecision is the audit detail of an approval decision: the verdict,
// the scope, and the tool it was about.
func auditDecision(approve bool, scope bridge.ApprovalScope, call *store.PendingToolCall) string {
	d := "reject"
	if approve {
		d = "approve scope=" + string(scope)
	}
	if call != nil && call.ToolName != "" {
		d += " tool=" + call.ToolName
	}
	return d
}
