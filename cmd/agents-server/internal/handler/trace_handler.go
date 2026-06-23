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
func (h *TraceHandler) ListBySession(c *gin.Context) {
	events, err := h.traces.ListBySession(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, events)
}
