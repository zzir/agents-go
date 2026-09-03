package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// TaskHandler serves the background tasks spawned from a chat session. Rows
// are the durable truth; live runs overlay their current status from the hub.
type TaskHandler struct {
	tasks  *store.TaskStore
	hub    *bridge.RunHub
	runner *bridge.Runner
}

// NewTaskHandler returns a handler backed by the task store and the runner
// (live status overlay, status-aware StopTask).
func NewTaskHandler(tasks *store.TaskStore, runner *bridge.Runner) *TaskHandler {
	return &TaskHandler{tasks: tasks, hub: runner.Hub(), runner: runner}
}

type taskStopReq struct {
	Graceful bool `json:"graceful"`
}

// Stop cancels a background task, with the same status-aware semantics as the
// model-facing task_stop tool: a live run is canceled (gracefully when asked),
// a task paused on an approval is canceled by discarding the approval, and an
// already-final task reports its actual status as an error.
//
//	@Summary		Stop a background task
//	@Description	Cancels a running task, or discards the pending approval of a paused one. Errors if the task is already final.
//	@Tags			tasks
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string		true	"Task ID"
//	@Param			request	body		taskStopReq	false	"Stop options"
//	@Success		200		{object}	bridge.TaskInfo
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/tasks/{id}/stop [post]
func (h *TaskHandler) Stop(c *gin.Context) {
	var req taskStopReq
	_ = c.ShouldBindJSON(&req) // body is optional
	info, err := h.runner.StopTask(c.Param("id"), req.Graceful)
	if err != nil {
		h.stopError(c, err)
		return
	}
	c.JSON(http.StatusOK, info)
}

// stopError maps a stop failure. The not-found sentinel is the SDK's, not
// store.ErrNotFound: every lookup goes through the task adapter.
func (h *TaskHandler) stopError(c *gin.Context, err error) {
	switch final, isFinal := errors.AsType[*bridge.TaskFinalError](err); {
	case isFinal:
		conflict(c, final.Error())
	case errors.Is(err, tasks.ErrNotFound), errors.Is(err, store.ErrNotFound):
		notFound(c)
	default:
		// A store failure is a server fault, not a resource conflict —
		// details go to the log, not the wire (error-envelope invariant).
		internalError(c, err)
	}
}

// Retry resumes a failed background task: the same task and session, a new
// run, continuing from where the failed attempt stopped.
//
//	@Summary		Retry a failed background task
//	@Description	Starts a new attempt at a failed task, resuming its existing conversation. Errors if the task is not failed or has used every attempt.
//	@Tags			tasks
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{object}	bridge.TaskInfo
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/tasks/{id}/retry [post]
func (h *TaskHandler) Retry(c *gin.Context) {
	info, err := h.runner.RetryTask(c.Param("id"))
	if err != nil {
		h.retryError(c, err)
		return
	}
	c.JSON(http.StatusOK, info)
}

// retryError maps a retry failure. A refusal by the task's own state (not
// failed, out of attempts, at a ceiling) is a 409 whose reason goes on the wire.
func (h *TaskHandler) retryError(c *gin.Context, err error) {
	_, notRetryable := errors.AsType[tasks.ErrNotRetryable](err)
	_, retryLimit := errors.AsType[tasks.ErrRetryLimit](err)
	_, taskLimit := errors.AsType[tasks.ErrTaskLimit](err)
	switch {
	case errors.Is(err, tasks.ErrNotFound), errors.Is(err, store.ErrNotFound):
		notFound(c)
	case notRetryable, retryLimit, taskLimit,
		errors.Is(err, tasks.ErrRetryConflict),
		errors.Is(err, store.ErrBudgetExhausted),
		errors.Is(err, store.ErrStepCeiling),
		errors.Is(err, store.ErrLoopBound):
		conflict(c, err.Error())
	default:
		internalError(c, err)
	}
}

// Dismiss hides a terminal task from the conversation's live strip.
//
//	@Summary		Dismiss a background task
//	@Description	Hides a finished task from the chat strip; the panel still lists it. Only terminal tasks can be dismissed, and a retry brings the task back.
//	@Tags			tasks
//	@Produce		json
//	@Param			id	path		string	true	"Task ID"
//	@Success		200	{object}	store.Task
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"still running"
//	@Security		BearerAuth
//	@Router			/tasks/{id}/dismiss [post]
func (h *TaskHandler) Dismiss(c *gin.Context) {
	ctx, id := c.Request.Context(), c.Param("id")
	won, err := h.tasks.Dismiss(ctx, id)
	if err != nil {
		internalError(c, err)
		return
	}
	t, err := h.tasks.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if !won && !bridge.IsTerminalTaskStatus(t.Status) {
		conflict(c, "a running task cannot be dismissed — stop it first")
		return
	}
	// Every window of the conversation hides it, not only the one that asked.
	if won {
		h.runner.AnnounceTask(ctx, id)
	}
	t.MaxAttempts = h.runner.MaxTaskAttempts()
	c.JSON(http.StatusOK, t)
}

// ListBySession responds with the session's background tasks, newest first.
//
//	@Summary		List background tasks
//	@Description	The session's background work — sub-agent tasks and workflow executions (kind "workflow", whose state carries the step sequence), both started through spawn_task — newest first. Status is live for running tasks and durable after they end.
//	@Tags			tasks
//	@Produce		json
//	@Param			id	path		string	true	"Session ID"
//	@Success		200	{array}		store.Task
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/tasks [get]
func (h *TaskHandler) ListBySession(c *gin.Context) {
	tasks, err := h.tasks.ListByParent(c.Request.Context(), c.Param("id"))
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range tasks {
		h.overlay(&tasks[i])
	}
	c.JSON(http.StatusOK, tasks)
}

// TaskPage is one page of the cross-session task list.
type TaskPage struct {
	Items []store.TaskWithSession `json:"items"`
	// Total is how many rows match the filter, for a pager.
	Total int `json:"total"`
}

// List responds with a page of background tasks across every conversation,
// newest first — the Workflows hub's Runs view.
//
//	@Summary		List background tasks across sessions
//	@Description	One page of the tasks of every live session, newest first, each with the name of the conversation it belongs to, plus the total. kind narrows to one kind ("workflow" for executions); live=true keeps only working / input_required rows; limit is the page size (500 at most), offset where the page starts.
//	@Tags			tasks
//	@Produce		json
//	@Param			kind	query		string	false	"Task kind: empty for sub-agent tasks and workflows alike, workflow for executions"
//	@Param			live	query		bool	false	"Only still-live tasks (working, input_required)"
//	@Param			limit	query		int		false	"Page size (default and maximum 500)"
//	@Param			offset	query		int		false	"Rows to skip"
//	@Success		200		{object}	TaskPage
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/tasks [get]
func (h *TaskHandler) List(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	u, _ := server.CurrentUser(c)
	rows, total, err := h.tasks.ListRecent(c.Request.Context(), u.ID, c.Query("kind"), c.Query("live") == "true", limit, offset)
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range rows {
		h.overlay(&rows[i].Task)
	}
	if rows == nil {
		rows = []store.TaskWithSession{}
	}
	c.JSON(http.StatusOK, TaskPage{Items: rows, Total: total})
}

// overlay stamps a row with the hub's live status (keyed by run id) and the
// retry ceiling. Terminal states stay the row's: the hub finishes before the row lands.
func (h *TaskHandler) overlay(t *store.Task) {
	t.MaxAttempts = h.runner.MaxTaskAttempts()
	if bridge.IsTerminalTaskStatus(t.Status) {
		return
	}
	if info, ok := h.hub.Info(t.RunID); ok {
		if hs := bridge.TaskStatusFor(info.Status); !bridge.IsTerminalTaskStatus(hs) {
			t.Status = hs
		}
	}
}
