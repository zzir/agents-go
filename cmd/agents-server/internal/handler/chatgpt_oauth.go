package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ChatGPTOAuthHandler exposes HTTP endpoints for a provider ChatGPT OAuth
// flow. All routes nest under the provider resource:
// /providers/:id/chatgpt/*, because the token is the ENDPOINT credential —
// every agent pointed at the provider shares the one login.
type ChatGPTOAuthHandler struct {
	oauth *providers.ChatGPTOAuth
}

// NewChatGPTOAuthHandler creates a handler backed by the given OAuth manager.
func NewChatGPTOAuthHandler(oauth *providers.ChatGPTOAuth) *ChatGPTOAuthHandler {
	return &ChatGPTOAuthHandler{oauth: oauth}
}

// Login starts the ChatGPT OAuth flow for the provider identified by the id
// path parameter and responds with the authorize URL.
//
//	@Summary		Start ChatGPT login
//	@Description	Starts the OAuth flow; open authorize_url in a browser. The callback is served by a temporary local server on port 1455.
//	@Tags			providers
//	@Produce		json
//	@Param			id	path		string	true	"Provider ID"
//	@Success		200	{object}	providers.ChatGPTLoginResult
//	@Failure		400	{object}	ErrorResponse	"provider does not use chatgpt_login"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/providers/{id}/chatgpt/login [post]
func (h *ChatGPTOAuthHandler) Login(c *gin.Context) {
	result, err := h.oauth.StartLogin(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(c)
			return
		}
		// A backend that doesn't offer chatgpt_login is the caller's
		// configuration problem, not a server fault.
		if errors.Is(err, providers.ErrChatGPTLoginUnavailable) {
			badRequest(c, err.Error())
			return
		}
		// The message is actionable local detail (e.g. callback port in use).
		abortError(c, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// chatgptStatusResp is the Status response.
type chatgptStatusResp struct {
	LoggedIn bool `json:"logged_in"`
}

// Status reports whether the provider identified by the id path parameter has a
// valid ChatGPT token.
//
//	@Summary	ChatGPT login status
//	@Tags		providers
//	@Produce	json
//	@Param		id	path		string	true	"Provider ID"
//	@Success	200	{object}	chatgptStatusResp
//	@Security	BearerAuth
//	@Router		/providers/{id}/chatgpt/status [get]
func (h *ChatGPTOAuthHandler) Status(c *gin.Context) {
	loggedIn, err := h.oauth.IsLoggedIn(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err) // ErrNotFound -> 404, else 500
		return
	}
	c.JSON(http.StatusOK, chatgptStatusResp{LoggedIn: loggedIn})
}

// Logout clears the ChatGPT token for the provider identified by the id path
// parameter.
//
//	@Summary	ChatGPT logout
//	@Tags		providers
//	@Param		id	path	string	true	"Provider ID"
//	@Success	204	"logged out"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/providers/{id}/chatgpt/logout [post]
func (h *ChatGPTOAuthHandler) Logout(c *gin.Context) {
	if err := h.oauth.Logout(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err) // ErrNotFound -> 404, else 500
		return
	}
	c.Status(http.StatusNoContent)
}
