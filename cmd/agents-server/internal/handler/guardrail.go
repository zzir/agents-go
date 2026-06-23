package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
)

// GuardrailHandler serves the catalog of available guardrails.
type GuardrailHandler struct{}

// NewGuardrailHandler returns a new guardrail handler.
func NewGuardrailHandler() *GuardrailHandler {
	return &GuardrailHandler{}
}

// List responds with all registered guardrails.
func (h *GuardrailHandler) List(c *gin.Context) {
	c.JSON(http.StatusOK, bridge.ListGuardrails())
}
