package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// WorkflowStarter starts an execution for a session with a brief; implemented
// by the bridge Runner.
type WorkflowStarter interface {
	RunWorkflow(ctx context.Context, workflowID, sessionID, input string, origin store.WorkflowOrigin) (*bridge.TaskInfo, error)
	// BindSessionSandbox gives a still-unbound session the project the start
	// carries, before the start; a bound session keeps its binding.
	BindSessionSandbox(ctx context.Context, sessionID, sandboxID, workDir string) (bool, error)
}

// WorkflowHandler serves the workflow DEFINITIONS and starts executions.
// Executions are tasks (kind "workflow"): once started they are listed,
// stopped, retried and dismissed through the task endpoints.
type WorkflowHandler struct {
	store   *store.WorkflowStore
	agents  *store.AgentConfigStore
	starter WorkflowStarter
}

// NewWorkflowHandler returns a handler backed by the given stores and starter.
func NewWorkflowHandler(s *store.WorkflowStore, agents *store.AgentConfigStore, starter WorkflowStarter) *WorkflowHandler {
	return &WorkflowHandler{store: s, agents: agents, starter: starter}
}

// runWorkflowReq is the body of a manual run: the session the result comes back
// to, and the brief — what this execution is about, written by the person.
type runWorkflowReq struct {
	SessionID string `json:"session_id" binding:"required"`
	Input     string `json:"input"`
	// SandboxID/WorkDir bind a still-unbound session first — the project the
	// composer had picked, or the dialog's — so the execution has its file and
	// command tools; a bound session ignores them, as a run request's does.
	SandboxID string `json:"sandbox_id"`
	WorkDir   string `json:"work_dir"`
}

// Run starts an execution of the workflow for a session, with a brief the
// person wrote — the same start the agent's spawn_task(workflow=…) makes.
//
//	@Summary		Run a workflow (optionally binding the session's project first)
//	@Description	Starts an execution as a background task of the given session, with the brief in the body (the workflow's session cannot see the conversation, so the brief is what it works from). The result comes back to the session as a notification, like any task's. 400 when the workflow has no runnable steps or an agent is gone, 404 for an unknown workflow or session, 409 when the session is at its background-task cap.
//	@Tags			workflows
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"Workflow ID"
//	@Param			request	body		runWorkflowReq	true	"Session and brief"
//	@Success		201		{object}	bridge.TaskInfo
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workflows/{id}/runs [post]
func (h *WorkflowHandler) Run(c *gin.Context) {
	var req runWorkflowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.SandboxID != "" {
		if _, err := h.starter.BindSessionSandbox(c.Request.Context(), req.SessionID, req.SandboxID, req.WorkDir); err != nil {
			switch {
			case errors.As(err, new(bridge.ErrInvalidBinding)):
				badRequest(c, err.Error())
			case errors.Is(err, bridge.ErrBindingContention): // transient: the config is being edited, retry
				conflict(c, err.Error())
			default:
				storeError(c, err)
			}
			return
		}
	}
	info, err := h.starter.RunWorkflow(c.Request.Context(), c.Param("id"), req.SessionID, strings.TrimSpace(req.Input), store.WorkflowOrigin{Kind: store.OriginPerson})
	if err != nil {
		switch {
		case errors.Is(err, bridge.ErrWorkflowUnavailable):
			badRequest(c, err.Error())
		case errors.As(err, new(tasks.ErrTaskLimit)):
			conflict(c, err.Error())
		default:
			storeError(c, err) // not-found (workflow or session) -> 404, anything else -> 500
		}
		return
	}
	c.JSON(http.StatusCreated, info)
}

// bind decodes and validates an incoming definition, reporting the failure
// itself. Every step's agent is checked here so a broken sequence is refused at
// save rather than halfway through a run.
func (h *WorkflowHandler) bind(c *gin.Context, wf *store.Workflow) bool {
	if err := c.ShouldBindJSON(wf); err != nil {
		badRequest(c, err.Error())
		return false
	}
	if err := store.NormalizeWorkflow(wf); err != nil {
		badRequest(c, err.Error())
		return false
	}
	for i := range wf.Steps {
		if _, err := h.agents.Get(c.Request.Context(), wf.Steps[i].AgentConfigID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				badRequest(c, "step "+wf.Steps[i].ID+": agent_config_id names no agent")
			} else {
				internalError(c, err)
			}
			return false
		}
	}
	return true
}

// List responds with every workflow definition.
//
//	@Summary	List workflows
//	@Tags		workflows
//	@Produce	json
//	@Success	200	{array}		store.Workflow
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/workflows [get]
func (h *WorkflowHandler) List(c *gin.Context) {
	list, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

// Get responds with one workflow definition.
//
//	@Summary	Get a workflow
//	@Tags		workflows
//	@Produce	json
//	@Param		id	path		string	true	"Workflow ID"
//	@Success	200	{object}	store.Workflow
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/workflows/{id} [get]
func (h *WorkflowHandler) Get(c *gin.Context) {
	wf, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, wf)
}

// Create stores a new workflow definition.
//
//	@Summary		Create a workflow
//	@Description	An ordered sequence of steps, each naming the agent that runs it and the prompt that starts its turn. A step without an id is given a stable one.
//	@Tags			workflows
//	@Accept			json
//	@Produce		json
//	@Param			workflow	body		store.Workflow	true	"Workflow"
//	@Success		201			{object}	store.Workflow
//	@Failure		400			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"duplicate name"
//	@Security		BearerAuth
//	@Router			/workflows [post]
func (h *WorkflowHandler) Create(c *gin.Context) {
	var wf store.Workflow
	if !h.bind(c, &wf) {
		return
	}
	wf.ID = "" // server-owned
	if err := h.store.Create(c.Request.Context(), &wf); err != nil {
		saveError(c, err) // duplicate name -> 409
		return
	}
	c.JSON(http.StatusCreated, wf)
}

// Update overwrites a workflow definition.
//
//	@Summary		Update a workflow
//	@Description	Executions already in flight are unaffected — each carries a snapshot of the definition it started with.
//	@Tags			workflows
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string			true	"Workflow ID"
//	@Param			workflow	body		store.Workflow	true	"Workflow"
//	@Success		200			{object}	store.Workflow
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"duplicate name"
//	@Security		BearerAuth
//	@Router			/workflows/{id} [put]
func (h *WorkflowHandler) Update(c *gin.Context) {
	var wf store.Workflow
	if !h.bind(c, &wf) {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	if err := h.store.Update(ctx, id, &wf); err != nil {
		saveError(c, err)
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete removes a workflow definition.
//
//	@Summary		Delete a workflow
//	@Description	Executions keep their snapshot, so past and in-flight runs still read and finish normally.
//	@Tags			workflows
//	@Param			id	path	string	true	"Workflow ID"
//	@Success		204
//	@Failure		404	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workflows/{id} [delete]
func (h *WorkflowHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
