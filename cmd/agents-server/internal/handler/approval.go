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
// and the runner.
func NewApprovalHandler(s *store.PendingApprovalStore, runner *bridge.Runner) *ApprovalHandler {
	return &ApprovalHandler{store: s, runner: runner}
}

// ListBySession responds with the pending approvals for the session
// identified by the id path parameter.
//
//	@Summary		List pending approvals
//	@Description	Tool calls the session is paused on, awaiting a human decision. Survives restarts.
//	@Tags			approvals
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		200	{array}		store.PendingApproval
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/approvals [get]
func (h *ApprovalHandler) ListBySession(c *gin.Context) {
	items, err := h.store.ListBySession(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, items)
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
	runID, err := h.runner.ResolveApproval(c.Request.Context(), toolCallID, approve, scope, reason, nil)
	if err != nil {
		h.resolveError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, approvalResultResp{RunID: runID, Status: string(bridge.RunRunning)})
}

func (h *ApprovalHandler) resolveError(c *gin.Context, err error) {
	var busy bridge.ErrSessionBusy
	switch {
	case errors.As(err, &busy):
		conflict(c, "session already has an active run: "+busy.RunID)
	case errors.Is(err, store.ErrNotFound):
		notFound(c)
	default:
		internalError(c, err)
	}
}
