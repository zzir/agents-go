package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// AuthHandler serves the authentication surface: config for the login page,
// token login (token mode), and the signed-in user's session endpoints.
type AuthHandler struct {
	svc *authn.Service
}

// NewAuthHandler returns an AuthHandler over the given service.
func NewAuthHandler(svc *authn.Service) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// Config reports how to authenticate, for the login page.
//
//	@Summary	Auth configuration
//	@Tags		auth
//	@Produce	json
//	@Success	200	{object}	protocol.AuthConfig
//	@Router		/auth/config [get]
func (h *AuthHandler) Config(c *gin.Context) {
	c.JSON(http.StatusOK, h.svc.ConfigView())
}

// Login validates the static token (token mode). In OAuth mode token login is
// disabled and answers 400.
//
//	@Summary	Token login
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Success	200	{object}	map[string]bool
//	@Failure	400	{object}	ErrorResponse	"malformed body, or token login disabled (OAuth mode)"
//	@Failure	401	{object}	ErrorResponse
//	@Router		/auth/login [post]
func (h *AuthHandler) Login(c *gin.Context) {
	if h.svc.Mode() != authn.ModeToken {
		c.JSON(http.StatusBadRequest, protocol.NewErrorResponse(protocol.CodeValidation, "token login is disabled; sign in through OAuth"))
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, protocol.NewErrorResponse(protocol.CodeValidation, "invalid request"))
		return
	}
	if !h.svc.StaticOK(req.Token) {
		c.JSON(http.StatusUnauthorized, protocol.NewErrorResponse(protocol.CodeUnauthorized, "invalid token"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Check validates the presented bearer — the stored-credential probe the SPA
// runs before showing the app.
//
//	@Summary	Validate the presented credential
//	@Tags		auth
//	@Produce	json
//	@Success	200	{object}	map[string]bool
//	@Failure	401	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/check [get]
func (h *AuthHandler) Check(c *gin.Context) {
	if _, err := h.svc.Authenticate(c.Request.Context(), server.BearerToken(c)); err != nil {
		c.JSON(http.StatusUnauthorized, protocol.NewErrorResponse(protocol.CodeUnauthorized, "unauthorized"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Me reports the authenticated caller.
//
//	@Summary	Current user
//	@Tags		auth
//	@Produce	json
//	@Success	200	{object}	protocol.UserInfo
//	@Failure	401	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/me [get]
func (h *AuthHandler) Me(c *gin.Context) {
	u, ok := server.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, protocol.NewErrorResponse(protocol.CodeUnauthorized, "unauthorized"))
		return
	}
	c.JSON(http.StatusOK, u)
}

// Logout revokes the presented session token (OAuth mode); a no-op success in
// token mode, where the SPA just clears its stored copy.
//
//	@Summary	Sign out
//	@Tags		auth
//	@Success	204	"signed out"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/logout [post]
func (h *AuthHandler) Logout(c *gin.Context) {
	if err := h.svc.Logout(c.Request.Context(), server.BearerToken(c)); err != nil {
		internalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
