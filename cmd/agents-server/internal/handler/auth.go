package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// AuthHandler serves the authentication surface: config for the login page,
// token login (token mode), the signed-in user's session endpoints, personal
// access tokens, and the admin's user management.
type AuthHandler struct {
	svc    *authn.Service
	tokens *store.AuthTokenStore
	users  *store.UserStore
	audit  *store.AuditStore
	// Conns closes a user's live WebSocket connections when a credential
	// or role they were opened under goes away (nil: nothing to close).
	Conns *server.ConnTracker
}

// NewAuthHandler returns an AuthHandler over the given service and stores
// (a store may be nil in tests that never reach its endpoints).
func NewAuthHandler(svc *authn.Service, tokens *store.AuthTokenStore, users *store.UserStore, audit *store.AuditStore) *AuthHandler {
	return &AuthHandler{svc: svc, tokens: tokens, users: users, audit: audit}
}

// ListAudit pages the audit log newest first — the admin's "who did what".
//
//	@Summary	Audit log (admin)
//	@Tags		auth
//	@Produce	json
//	@Param		limit	query		int		false	"Page size (default 100, max 500)"
//	@Param		before	query		string	false	"An event id; the page before it"
//	@Success	200		{array}		store.AuditEvent
//	@Failure	403		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/audit [get]
func (h *AuthHandler) ListAudit(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	list, err := h.audit.ListRecent(c.Request.Context(), limit, c.Query("before"))
	if err != nil {
		internalError(c, err)
		return
	}
	if list == nil {
		list = []store.AuditEvent{}
	}
	c.JSON(http.StatusOK, list)
}

// ListUsers lists every account — the admin's user management view.
//
//	@Summary	List users (admin)
//	@Tags		auth
//	@Produce	json
//	@Success	200	{array}		store.User
//	@Failure	403	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/users [get]
func (h *AuthHandler) ListUsers(c *gin.Context) {
	list, err := h.users.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	if list == nil {
		list = []store.User{}
	}
	c.JSON(http.StatusOK, list)
}

// PatchUser changes an account's role or switches it off. The store refuses
// the change that would leave no enabled admin (409); the local account is
// not a person to manage; an admin acting on themself is refused outright,
// since a locked-out admin's only recovery is --bootstrap-admin.
//
//	@Summary	Change a user's role or disable them (admin)
//	@Tags		auth
//	@Accept		json
//	@Param		id		path	string				true	"User ID"
//	@Param		body	body	UserPatchRequest	true	"What to change"
//	@Success	204		"changed"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	403		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	409		{object}	ErrorResponse	"that would leave no admin"
//	@Security	BearerAuth
//	@Router		/auth/users/{id} [patch]
func (h *AuthHandler) PatchUser(c *gin.Context) {
	var req UserPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil || (req.Role == nil && req.Disabled == nil) {
		badRequest(c, "role and/or disabled are required")
		return
	}
	if req.Role != nil && *req.Role != store.RoleAdmin && *req.Role != store.RoleMember {
		badRequest(c, "role must be admin or member")
		return
	}
	id := c.Param("id")
	if id == store.LocalUserID {
		badRequest(c, "the local account is not managed")
		return
	}
	if me, _ := server.CurrentUser(c); me.ID == id {
		badRequest(c, "you cannot change your own account")
		return
	}
	var detail []string
	if req.Role != nil {
		detail = append(detail, "role="+*req.Role)
	}
	if req.Disabled != nil {
		detail = append(detail, "disabled="+strconv.FormatBool(*req.Disabled))
	}
	server.SetAuditDetail(c, strings.Join(detail, " "))
	err := h.users.Patch(c.Request.Context(), id, store.UserPatch{Role: req.Role, Disabled: req.Disabled})
	if errors.Is(err, store.ErrLastAdmin) {
		conflict(c, err.Error())
		return
	}
	if err != nil {
		storeError(c, err)
		return
	}
	// What the old role or the credentials opened — a terminal above all —
	// closes; a still-enabled client reconnects as what it is now.
	h.Conns.CloseForUser(id, "account changed")
	if req.Disabled != nil && *req.Disabled {
		if _, err := h.tokens.RevokeAllForUser(c.Request.Context(), id); err != nil {
			internalError(c, err)
			return
		}
	}
	c.Status(http.StatusNoContent)
}

// UserPatchRequest is the body of PATCH /auth/users/:id.
type UserPatchRequest struct {
	Role     *string `json:"role,omitempty"`
	Disabled *bool   `json:"disabled,omitempty"`
}

// RevokeUserTokens signs one account out everywhere: every session and PAT
// of theirs is deleted and their live connections closed.
//
//	@Summary	Revoke every token of a user (admin)
//	@Tags		auth
//	@Param		id	path	string	true	"User ID"
//	@Success	204	"revoked"
//	@Failure	403	{object}	ErrorResponse
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/users/{id}/tokens [delete]
func (h *AuthHandler) RevokeUserTokens(c *gin.Context) {
	id := c.Param("id")
	if _, err := h.users.ByID(c.Request.Context(), id); err != nil {
		storeError(c, err)
		return
	}
	n, err := h.tokens.RevokeAllForUser(c.Request.Context(), id)
	if err != nil {
		internalError(c, err)
		return
	}
	server.SetAuditDetail(c, "tokens="+strconv.FormatInt(n, 10))
	h.Conns.CloseForUser(id, "signed out by an admin")
	c.Status(http.StatusNoContent)
}

// requirePATMode gates the PAT endpoints: in token mode a PAT could be minted
// but never authenticate (the static compare is the whole check), so refusing
// is honest where accepting would hand out dead credentials.
func (h *AuthHandler) requirePATMode(c *gin.Context) (protocol.UserInfo, bool) {
	if h.svc.Mode() != authn.ModeOAuth {
		c.JSON(http.StatusBadRequest, protocol.NewErrorResponse(protocol.CodeValidation,
			"personal access tokens require --auth oauth; token mode authenticates with the static token"))
		return protocol.UserInfo{}, false
	}
	u, ok := server.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, protocol.NewErrorResponse(protocol.CodeUnauthorized, "unauthorized"))
		return protocol.UserInfo{}, false
	}
	return u, true
}

func patView(t *store.AuthToken) protocol.PatView {
	v := protocol.PatView{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt.Format(time.RFC3339)}
	if !t.LastUsedAt.IsZero() {
		v.LastUsedAt = t.LastUsedAt.Format(time.RFC3339)
	}
	if !t.ExpiresAt.IsZero() {
		v.ExpiresAt = t.ExpiresAt.Format(time.RFC3339)
	}
	return v
}

// ListTokens lists the caller's personal access tokens — labels and dates,
// never secrets.
//
//	@Summary	List personal access tokens
//	@Tags		auth
//	@Produce	json
//	@Success	200	{array}		protocol.PatView
//	@Failure	400	{object}	ErrorResponse	"token mode"
//	@Security	BearerAuth
//	@Router		/auth/tokens [get]
func (h *AuthHandler) ListTokens(c *gin.Context) {
	u, ok := h.requirePATMode(c)
	if !ok {
		return
	}
	list, err := h.tokens.ListByUser(c.Request.Context(), u.ID, store.TokenKindPAT)
	if err != nil {
		internalError(c, err)
		return
	}
	out := make([]protocol.PatView, 0, len(list))
	for i := range list {
		out = append(out, patView(&list[i]))
	}
	c.JSON(http.StatusOK, out)
}

// CreateToken mints a personal access token. The response carries the
// plaintext once; it is not retrievable afterwards.
//
//	@Summary	Create a personal access token
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Success	201	{object}	protocol.PatCreated
//	@Failure	400	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/tokens [post]
func (h *AuthHandler) CreateToken(c *gin.Context) {
	u, ok := h.requirePATMode(c)
	if !ok {
		return
	}
	var req struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" || req.ExpiresInDays < 0 {
		c.JSON(http.StatusBadRequest, protocol.NewErrorResponse(protocol.CodeValidation,
			"a token needs a name; expires_in_days is optional (0 = never)"))
		return
	}
	var expires time.Time
	if req.ExpiresInDays > 0 {
		expires = time.Now().UTC().AddDate(0, 0, req.ExpiresInDays)
	}
	secret, minted, err := h.tokens.Mint(c.Request.Context(), u.ID, store.TokenKindPAT, strings.TrimSpace(req.Name), expires)
	if err != nil {
		internalError(c, err)
		return
	}
	server.SetAuditResource(c, minted.ID)
	c.JSON(http.StatusCreated, protocol.PatCreated{Token: secret, Pat: patView(minted)})
}

// DeleteToken revokes one of the caller's personal access tokens.
//
//	@Summary	Revoke a personal access token
//	@Tags		auth
//	@Param		id	path	string	true	"Token ID"
//	@Success	204	"revoked"
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/tokens/{id} [delete]
func (h *AuthHandler) DeleteToken(c *gin.Context) {
	u, ok := h.requirePATMode(c)
	if !ok {
		return
	}
	if err := h.tokens.Revoke(c.Request.Context(), c.Param("id"), u.ID); err != nil {
		storeError(c, err)
		return
	}
	// A connection the revoked token opened closes; those the user's other
	// credentials opened reconnect and carry on.
	h.Conns.CloseForUser(u.ID, "token revoked")
	c.Status(http.StatusNoContent)
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
	if u, err := h.svc.Authenticate(c.Request.Context(), req.Token); err == nil {
		server.SetAuditActor(c, u) // a login is the one line worth having on this route
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Check is the SPA's stored-credential probe: an authenticated route (the
// middleware did the check) that answers nothing but ok.
//
//	@Summary	Validate the presented credential
//	@Tags		auth
//	@Produce	json
//	@Success	200	{object}	map[string]bool
//	@Failure	401	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/check [get]
func (h *AuthHandler) Check(c *gin.Context) {
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

// OAuthStart redirects the browser into the named provider's authorize flow.
//
//	@Summary	Begin an OAuth login
//	@Tags		auth
//	@Param		provider	path	string	true	"Provider name (e.g. google)"
//	@Success	302			"redirect to the provider's authorize URL"
//	@Failure	404			{object}	ErrorResponse	"unknown provider (or token mode)"
//	@Router		/auth/oauth/{provider}/start [get]
func (h *AuthHandler) OAuthStart(c *gin.Context) {
	authURL, nonce, err := h.svc.Begin(c.Param("provider"))
	if err != nil {
		notFound(c)
		return
	}
	// The browser's half of the login: HttpOnly so no script reads it, Lax
	// so the provider's top-level redirect back still carries it, and as
	// short-lived as the pending login itself.
	name, secure := h.svc.LoginCookie()
	http.SetCookie(c.Writer, &http.Cookie{
		Name: name, Value: nonce, Path: "/", MaxAge: int(authn.LoginTTL / time.Second),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	c.Redirect(http.StatusFound, authURL)
}

// OAuthCallback finishes a login. It always redirects into the SPA — success
// carries a one-time exchange code in the URL fragment, failure a coarse
// error tag; the detail is logged, not shown to an unauthenticated visitor.
//
//	@Summary	OAuth provider callback
//	@Tags		auth
//	@Param		provider	path	string	true	"Provider name"
//	@Param		state		query	string	true	"Authorize round-trip state"
//	@Param		code		query	string	false	"Authorization code"
//	@Param		error		query	string	false	"The provider's error (a cancelled consent)"
//	@Success	302			"redirect into the SPA"
//	@Router		/auth/oauth/{provider}/callback [get]
func (h *AuthHandler) OAuthCallback(c *gin.Context) {
	name, secure := h.svc.LoginCookie()
	nonce, _ := c.Cookie(name)
	http.SetCookie(c.Writer, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	redirect := h.svc.Complete(c.Request.Context(), c.Param("provider"), c.Query("state"), c.Query("code"), nonce, c.Query("error"))
	c.Redirect(http.StatusFound, redirect)
}

// Exchange trades the callback's one-time code for the session token — the
// only response the token plaintext ever rides.
//
//	@Summary	Exchange the one-time login code for a session token
//	@Tags		auth
//	@Accept		json
//	@Produce	json
//	@Success	200	{object}	protocol.AuthSession
//	@Failure	400	{object}	ErrorResponse
//	@Failure	401	{object}	ErrorResponse	"unknown, used, or expired code"
//	@Router		/auth/exchange [post]
func (h *AuthHandler) Exchange(c *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		c.JSON(http.StatusBadRequest, protocol.NewErrorResponse(protocol.CodeValidation, "invalid request"))
		return
	}
	token, user, ok := h.svc.Exchange(req.Code)
	if !ok {
		c.JSON(http.StatusUnauthorized, protocol.NewErrorResponse(protocol.CodeUnauthorized, "unknown or expired code"))
		return
	}
	server.SetAuditActor(c, user) // the exchange IS the completed login
	c.JSON(http.StatusOK, protocol.AuthSession{Token: token, User: user})
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
	if u, ok := server.CurrentUser(c); ok {
		h.Conns.CloseForUser(u.ID, "signed out")
	}
	c.Status(http.StatusNoContent)
}
