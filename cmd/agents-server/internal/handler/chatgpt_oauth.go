package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ChatGPTOAuthHandler exposes HTTP endpoints for a provider ChatGPT OAuth
// flow. All routes nest under the provider resource:
// /providers/:id/chatgpt/*, because the token is the ENDPOINT credential —
// every agent pointed at the provider shares the one login.
type ChatGPTOAuthHandler struct {
	oauth     *providers.ChatGPTOAuth
	providers *store.ProviderStore
}

// NewChatGPTOAuthHandler creates a handler backed by the given OAuth manager;
// the provider store answers whose row the login belongs to.
func NewChatGPTOAuthHandler(oauth *providers.ChatGPTOAuth, providerStore *store.ProviderStore) *ChatGPTOAuthHandler {
	return &ChatGPTOAuthHandler{oauth: oauth, providers: providerStore}
}

// editable loads the provider and gates a login/logout on it: signing a
// private provider into ChatGPT is its owner's act, a global one an admin's.
func (h *ChatGPTOAuthHandler) editable(c *gin.Context) bool {
	pv, err := h.providers.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return false
	}
	return editableRow(c, pv.Scope, pv.OwnerID)
}

// Login starts the ChatGPT OAuth flow for the provider identified by the id
// path parameter and responds with the authorize URL.
//
//	@Summary		Start ChatGPT login
//	@Description	Starts the OAuth flow; open authorize_url in a browser, authorize, then submit the resulting callback URL to /chatgpt/complete.
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
	if !h.editable(c) {
		return
	}
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
		internalError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// chatgptCompleteReq carries the callback URL the user pastes after authorizing.
type chatgptCompleteReq struct {
	RedirectURL string `json:"redirect_url"`
}

// Complete finishes the ChatGPT OAuth flow by redeeming the callback URL the
// user pasted (decisions §5.41).
//
//	@Summary		Complete ChatGPT login
//	@Description	Redeems the callback URL the user pastes after authorizing (accepts the full URL or its query string).
//	@Tags			providers
//	@Accept			json
//	@Param			id		path	string				true	"Provider ID"
//	@Param			request	body	chatgptCompleteReq	true	"Pasted callback URL"
//	@Success		204		"logged in"
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/providers/{id}/chatgpt/complete [post]
func (h *ChatGPTOAuthHandler) Complete(c *gin.Context) {
	if !h.editable(c) {
		return
	}
	var req chatgptCompleteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "redirect_url is required")
		return
	}
	err := h.oauth.CompleteLogin(c.Request.Context(), c.Param("id"), req.RedirectURL)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		notFound(c)
	case errors.Is(err, providers.ErrChatGPTLoginUnavailable),
		errors.Is(err, providers.ErrChatGPTLoginExpired),
		errors.Is(err, providers.ErrChatGPTCallbackInvalid):
		badRequest(c, err.Error())
	default:
		internalError(c, err)
	}
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
	if !h.editable(c) {
		return
	}
	if err := h.oauth.Logout(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err) // ErrNotFound -> 404, else 500
		return
	}
	c.Status(http.StatusNoContent)
}
