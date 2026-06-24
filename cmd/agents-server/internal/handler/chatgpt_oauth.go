package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
)

type ChatGPTOAuthHandler struct {
	oauth *bridge.ChatGPTOAuth
}

func NewChatGPTOAuthHandler(oauth *bridge.ChatGPTOAuth) *ChatGPTOAuthHandler {
	return &ChatGPTOAuthHandler{oauth: oauth}
}

func (h *ChatGPTOAuthHandler) Login(c *gin.Context) {
	agentID := c.Query("agent_config_id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_config_id is required"})
		return
	}
	result, err := h.oauth.StartLogin(agentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *ChatGPTOAuthHandler) Callback(c *gin.Context) {
	c.String(http.StatusOK, "Callback handled by temporary server on port 1455")
}

func (h *ChatGPTOAuthHandler) Status(c *gin.Context) {
	agentID := c.Query("agent_config_id")
	loggedIn := h.oauth.IsLoggedIn(c.Request.Context(), agentID)
	c.JSON(http.StatusOK, gin.H{"logged_in": loggedIn})
}

func (h *ChatGPTOAuthHandler) Logout(c *gin.Context) {
	agentID := c.Query("agent_config_id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_config_id is required"})
		return
	}
	if err := h.oauth.Logout(c.Request.Context(), agentID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
