package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// TraceHandler serves trace events recorded for sessions.
type TraceHandler struct {
	traces *store.TraceStore
}

// NewTraceHandler returns a handler backed by the given trace store.
func NewTraceHandler(traces *store.TraceStore) *TraceHandler {
	return &TraceHandler{traces: traces}
}

// ListBySession responds with the trace events for the session identified by the id path parameter.
//
//	@Summary		List session traces
//	@Description	Without limit, returns all events oldest-first. With limit, returns the newest `limit` events (still oldest-first); page backwards by passing the smallest received id as before_id.
//	@Tags			sessions
//	@Produce		json
//	@Param			id			path		string	true	"Session ID"
//	@Param			limit		query		int		false	"Max events to return; 0 or absent returns all"
//	@Param			before_id	query		int		false	"Only events with id < before_id (backwards cursor)"
//	@Success		200			{array}		store.TraceEvent
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/traces [get]
func (h *TraceHandler) ListBySession(c *gin.Context) {
	beforeID, limit := pageParams(c)
	events, err := h.traces.ListBySession(c.Request.Context(), c.Param("id"), beforeID, limit)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, events)
}
