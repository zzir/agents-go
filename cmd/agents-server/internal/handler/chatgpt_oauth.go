package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
)

// The ChatGPT OAuth flow of a provider, under /providers/:id/chatgpt/*; the
// token is the endpoint's, shared by its agents.

// chatgptEditable gates a login/logout: signing a private provider into
// ChatGPT is its owner's act, a global one an admin's.
func (h *ProviderHandler) chatgptEditable(c *gin.Context) error {
	_, err := gatedRow(c, h.store.CrudStore, providerScope, editableRow)
	return err
}

// ChatGPTLogin starts the ChatGPT OAuth flow for the provider identified by the id
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
func (h *ProviderHandler) ChatGPTLogin(c *gin.Context) error {
	if err := h.chatgptEditable(c); err != nil {
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

// ChatGPTComplete finishes the ChatGPT OAuth flow — decisions §5.41.
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
func (h *ProviderHandler) ChatGPTComplete(c *gin.Context) error {
	if err := h.chatgptEditable(c); err != nil {
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

// ChatGPTLogout clears the ChatGPT token for the provider identified by the id path
// parameter.
//
//	@Summary	ChatGPT logout
//	@Tags		providers
//	@Param		id	path	string	true	"Provider ID"
//	@Success	204	"logged out"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/providers/{id}/chatgpt/logout [post]
func (h *ProviderHandler) ChatGPTLogout(c *gin.Context) error {
	if err := h.chatgptEditable(c); err != nil {
		return err
	}
	if err := h.oauth.Logout(c.Request.Context(), c.Param("id")); err != nil {
		return err // ErrNotFound -> 404, else 500
	}
	c.Status(http.StatusNoContent)
	return nil
}
