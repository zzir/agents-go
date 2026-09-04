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
//	@Description	Without limit, returns all events oldest-first. With limit, returns the newest `limit` events (still oldest-first); page backwards by passing the smallest received id as before_id. `summary=true` leaves the payload fields (input, output, system_instructions, tools, handoffs, output_schema) out of each row's data — they are nearly all of a session's trace bytes — and marks such rows `payload_omitted`; GET /sessions/{id}/traces/{span_id} serves one span whole.
//	@Tags			sessions
//	@Produce		json
//	@Param			id			path		string	true	"Session ID"
//	@Param			limit		query		int		false	"Max events to return; 0 or absent returns all"
//	@Param			before_id	query		int		false	"Only events with id < before_id (backwards cursor)"
//	@Param			summary		query		bool	false	"Leave the payload fields out of data (rows marked payload_omitted)"
//	@Success		200			{array}		store.TraceEvent
//	@Failure		500			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/traces [get]
func (h *TraceHandler) ListBySession(c *gin.Context) {
	beforeID, limit := pageParams(c)
	list := h.traces.ListBySession
	if c.Query("summary") == "true" {
		list = h.traces.ListSummaryBySession
	}
	events, err := list(c.Request.Context(), c.Param("id"), beforeID, limit)
	if err != nil {
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, nonNilList(events))
}

// GetBySpan responds with one span of the session, payload included.
//
//	@Summary		Get one trace span
//	@Description	The whole row of one span — what a `summary=true` listing left out (`payload_omitted`), or what the live cap replaced with a marker on the WebSocket.
//	@Tags			sessions
//	@Produce		json
//	@Param			id		path		string	true	"Session ID"
//	@Param			span_id	path		string	true	"Span ID"
//	@Success		200		{object}	store.TraceEvent
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sessions/{id}/traces/{span_id} [get]
func (h *TraceHandler) GetBySpan(c *gin.Context) {
	ev, err := h.traces.GetBySpan(c.Request.Context(), c.Param("id"), c.Param("span_id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, ev)
}
