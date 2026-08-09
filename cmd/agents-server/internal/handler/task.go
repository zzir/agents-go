package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
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
// (whose hub overlays live status and whose StopTask carries the
// status-aware cancel semantics).
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

// stopError maps a stop failure. The not-found sentinel is the SDK's, not the
// store's: every lookup goes through the task adapter, which maps its own to
// that one — matching store.ErrNotFound here caught nothing, so an unknown id
// used to answer 500.
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

// retryError maps a retry failure. A refusal by the task's own state — it is
// not failed, it is out of attempts, or its session is at the live-task
// ceiling — is a conflict rather than a fault, and its reason goes on the wire
// because the caller can act on it.
func (h *TaskHandler) retryError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, tasks.ErrNotFound), errors.Is(err, store.ErrNotFound):
		notFound(c)
	case errors.As(err, new(tasks.ErrNotRetryable)),
		errors.As(err, new(tasks.ErrRetryLimit)),
		errors.As(err, new(tasks.ErrTaskLimit)),
		errors.Is(err, tasks.ErrRetryConflict):
		conflict(c, err.Error())
	default:
		internalError(c, err)
	}
}

// ListBySession responds with the session's background tasks, newest first.
//
//	@Summary		List background tasks
//	@Description	Tasks spawned from this session via spawn_task, newest first. Status is live for running tasks and durable after they end.
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
		// The hub tracks RUNS, so the overlay keys on the task's run id — its
		// task id is a different namespace and never matched, leaving this
		// overlay dead. Terminal states stay the row's: the hub finishes a run
		// before the row lands, and "completed" in that window would show a
		// task whose result is not readable yet.
		if bridge.IsTerminalTaskStatus(tasks[i].Status) {
			continue
		}
		if info, ok := h.hub.Info(tasks[i].RunID); ok {
			if hs := bridge.TaskStatusFor(info.Status); !bridge.IsTerminalTaskStatus(hs) {
				tasks[i].Status = hs
			}
		}
	}
	// The ceiling travels with every row, so a client can answer "could this be
	// retried" from the status it already tracks live.
	ceiling := h.runner.MaxTaskAttempts()
	for i := range tasks {
		tasks[i].MaxAttempts = ceiling
	}
	c.JSON(http.StatusOK, tasks)
}
