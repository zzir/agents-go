package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ChatGPTOAuthHandler exposes HTTP endpoints for per-agent ChatGPT OAuth.
// All routes are nested under the agent resource: /agents/:id/chatgpt/*.
type ChatGPTOAuthHandler struct {
	oauth *bridge.ChatGPTOAuth
}

// NewChatGPTOAuthHandler creates a handler backed by the given OAuth manager.
func NewChatGPTOAuthHandler(oauth *bridge.ChatGPTOAuth) *ChatGPTOAuthHandler {
	return &ChatGPTOAuthHandler{oauth: oauth}
}

// Login starts the ChatGPT OAuth flow for the agent identified by the id
// path parameter and responds with the authorize URL.
//
//	@Summary		Start ChatGPT login
//	@Description	Starts the OAuth flow; open authorize_url in a browser. The callback is served by a temporary local server on port 1455.
//	@Tags			agents
//	@Produce		json
//	@Param			id	path		string	true	"Agent ID"
//	@Success		200	{object}	bridge.ChatGPTLoginResult
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/agents/{id}/chatgpt/login [post]
func (h *ChatGPTOAuthHandler) Login(c *gin.Context) {
	result, err := h.oauth.StartLogin(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(c)
			return
		}
		// The message is actionable local detail (e.g. callback port in use).
		abortError(c, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// Callback is a placeholder; the real callback is handled by the temporary
// server on the fixed ChatGPT OAuth port.
func (h *ChatGPTOAuthHandler) Callback(c *gin.Context) {
	c.String(http.StatusOK, "Callback handled by temporary server on port 1455")
}

// chatgptStatusResp is the Status response.
type chatgptStatusResp struct {
	LoggedIn bool `json:"logged_in"`
}

// Status reports whether the agent identified by the id path parameter has a
// valid ChatGPT token.
//
//	@Summary	ChatGPT login status
//	@Tags		agents
//	@Produce	json
//	@Param		id	path		string	true	"Agent ID"
//	@Success	200	{object}	chatgptStatusResp
//	@Security	BearerAuth
//	@Router		/agents/{id}/chatgpt/status [get]
func (h *ChatGPTOAuthHandler) Status(c *gin.Context) {
	loggedIn, err := h.oauth.IsLoggedIn(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err) // ErrNotFound -> 404, else 500
		return
	}
	c.JSON(http.StatusOK, chatgptStatusResp{LoggedIn: loggedIn})
}

// Logout clears the ChatGPT token for the agent identified by the id path
// parameter.
//
//	@Summary	ChatGPT logout
//	@Tags		agents
//	@Param		id	path	string	true	"Agent ID"
//	@Success	204	"logged out"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/agents/{id}/chatgpt/logout [post]
func (h *ChatGPTOAuthHandler) Logout(c *gin.Context) {
	if err := h.oauth.Logout(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err) // ErrNotFound -> 404, else 500
		return
	}
	c.Status(http.StatusNoContent)
}
