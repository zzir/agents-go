package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

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
		var final *bridge.TaskFinalError
		switch {
		case errors.As(err, &final):
			conflict(c, final.Error())
		case errors.Is(err, store.ErrNotFound):
			notFound(c)
		default:
			// A store failure is a server fault, not a resource conflict —
			// details go to the log, not the wire (error-envelope invariant).
			internalError(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, info)
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
		if info, ok := h.hub.Info(tasks[i].ID); ok {
			tasks[i].Status = bridge.TaskStatusFor(info.Status)
		}
	}
	c.JSON(http.StatusOK, tasks)
}
