package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ChatGPTOAuthHandler serves a provider's ChatGPT OAuth flow under
// /providers/:id/chatgpt/*; the token is the endpoint's, shared by its agents.
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
func (h *ChatGPTOAuthHandler) editable(c *gin.Context) error {
	pv, err := h.providers.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
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
func (h *ChatGPTOAuthHandler) Login(c *gin.Context) error {
	if err := h.editable(c); err != nil {
		return err
	}
	result, err := h.oauth.StartLogin(c.Request.Context(), c.Param("id"))
	if err != nil {
		// A backend without chatgpt_login is the caller's configuration problem: 400.
		if errors.Is(err, providers.ErrChatGPTLoginUnavailable) {
			return badRequest(err.Error())
		}
		return err
	}
	c.JSON(http.StatusOK, result)
	return nil
}

// chatgptCompleteReq carries the callback URL the user pastes after authorizing.
type chatgptCompleteReq struct {
	RedirectURL string `json:"redirect_url"`
}

// Complete finishes the ChatGPT OAuth flow — decisions §5.41.
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
func (h *ChatGPTOAuthHandler) Complete(c *gin.Context) error {
	if err := h.editable(c); err != nil {
		return err
	}
	var req chatgptCompleteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		return badRequest("redirect_url is required")
	}
	err := h.oauth.CompleteLogin(c.Request.Context(), c.Param("id"), req.RedirectURL)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, providers.ErrChatGPTLoginUnavailable),
		errors.Is(err, providers.ErrChatGPTLoginExpired),
		errors.Is(err, providers.ErrChatGPTCallbackInvalid):
		return badRequest(err.Error())
	default:
		return err
	}
	return nil
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
func (h *ChatGPTOAuthHandler) Logout(c *gin.Context) error {
	if err := h.editable(c); err != nil {
		return err
	}
	if err := h.oauth.Logout(c.Request.Context(), c.Param("id")); err != nil {
		return err // ErrNotFound -> 404, else 500
	}
	c.Status(http.StatusNoContent)
	return nil
}
