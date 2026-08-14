package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// WorkflowDriver stops and retries executions; implemented by the bridge
// Runner. Starting one is deliberately NOT here: the brief has to be written by
// something that read the conversation, so the only way in is the agent's
// start_workflow tool.
type WorkflowDriver interface {
	StopWorkflow(workflowRunID string) (*store.WorkflowRun, error)
	RetryWorkflow(workflowRunID string) (*store.WorkflowRun, error)
}

// WorkflowHandler serves the workflow definitions and their executions.
type WorkflowHandler struct {
	store  *store.WorkflowStore
	runs   *store.WorkflowRunStore
	agents *store.AgentConfigStore
	driver WorkflowDriver
}

// NewWorkflowHandler returns a handler backed by the given stores and driver.
func NewWorkflowHandler(s *store.WorkflowStore, runs *store.WorkflowRunStore, agents *store.AgentConfigStore, driver WorkflowDriver) *WorkflowHandler {
	return &WorkflowHandler{store: s, runs: runs, agents: agents, driver: driver}
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

// ListBySession responds with the session's workflow executions, newest first.
//
//	@Summary	List a session's workflow runs
//	@Tags		workflows
//	@Produce	json
//	@Param		id	path		string	true	"Session ID"
//	@Success	200	{array}		store.WorkflowRun
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sessions/{id}/workflow-runs [get]
func (h *WorkflowHandler) ListBySession(c *gin.Context) {
	list, err := h.runs.ListBySession(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

// GetRun responds with one execution.
//
//	@Summary	Get a workflow run
//	@Tags		workflows
//	@Produce	json
//	@Param		id	path		string	true	"Workflow run ID"
//	@Success	200	{object}	store.WorkflowRun
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/workflow-runs/{id} [get]
func (h *WorkflowHandler) GetRun(c *gin.Context) {
	wr, err := h.runs.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, wr)
}

// StopRun cancels the whole execution.
//
//	@Summary		Stop a workflow run
//	@Description	Ends the sequence, not just the running step — the next step does not start.
//	@Tags			workflows
//	@Produce		json
//	@Param			id	path		string	true	"Workflow run ID"
//	@Success		200	{object}	store.WorkflowRun
//	@Failure		404	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workflow-runs/{id}/stop [post]
func (h *WorkflowHandler) StopRun(c *gin.Context) {
	wr, err := h.driver.StopWorkflow(c.Param("id"))
	if err != nil {
		h.driverError(c, err)
		return
	}
	c.JSON(http.StatusOK, wr)
}

// DismissRun hides a terminal execution from the conversation's live strip.
//
//	@Summary		Dismiss a workflow run
//	@Description	Hides a finished execution from the chat strip; the panel still lists it. Only terminal runs can be dismissed, and a retry brings the run back.
//	@Tags			workflows
//	@Produce		json
//	@Param			id	path		string	true	"Workflow run ID"
//	@Success		200	{object}	store.WorkflowRun
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"still running"
//	@Security		BearerAuth
//	@Router			/workflow-runs/{id}/dismiss [post]
func (h *WorkflowHandler) DismissRun(c *gin.Context) {
	ctx, id := c.Request.Context(), c.Param("id")
	won, err := h.runs.Dismiss(ctx, id)
	if err != nil {
		internalError(c, err)
		return
	}
	wr, err := h.runs.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if !won && wr.Status == store.WorkflowRunning {
		conflict(c, "a running workflow cannot be dismissed — stop it first")
		return
	}
	c.JSON(http.StatusOK, wr)
}

// RetryRun re-runs a terminal execution from the step it stopped at.
//
//	@Summary		Retry a workflow run
//	@Description	Resumes from the step it stopped at, keeping the steps that already succeeded. Executes the snapshot the run started with.
//	@Tags			workflows
//	@Produce		json
//	@Param			id	path		string	true	"Workflow run ID"
//	@Success		200	{object}	store.WorkflowRun
//	@Failure		400	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"session already has a live run"
//	@Security		BearerAuth
//	@Router			/workflow-runs/{id}/retry [post]
func (h *WorkflowHandler) RetryRun(c *gin.Context) {
	wr, err := h.driver.RetryWorkflow(c.Param("id"))
	if err != nil {
		h.driverError(c, err)
		return
	}
	c.JSON(http.StatusOK, wr)
}

// driverError maps the driver's typed refusals to status codes.
func (h *WorkflowHandler) driverError(c *gin.Context, err error) {
	if busy, ok := errors.AsType[bridge.ErrSessionBusy](err); ok {
		conflict(c, busy.Error())
		return
	}
	if errors.Is(err, bridge.ErrWorkflowUnavailable) {
		badRequest(c, err.Error())
		return
	}
	storeError(c, err) // not-found -> 404, anything else -> 500
}
