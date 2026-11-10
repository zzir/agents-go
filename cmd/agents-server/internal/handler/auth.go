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

// AuthHandler serves the authentication surface: login, the signed-in user's
// session endpoints, personal access tokens, and the admin's user management.
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
//	@Failure	400		{object}	ErrorResponse	"limit is not a non-negative integer"
//	@Failure	403		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/audit [get]
func (h *AuthHandler) ListAudit(c *gin.Context) error {
	limit, err := queryInt(c, "limit")
	if err != nil {
		return err
	}
	list, err := h.audit.ListRecent(c.Request.Context(), limit, c.Query("before"))
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.AuditEvent{}
	}
	c.JSON(http.StatusOK, list)
	return nil
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
func (h *AuthHandler) ListUsers(c *gin.Context) error {
	list, err := h.users.List(c.Request.Context())
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.User{}
	}
	c.JSON(http.StatusOK, list)
	return nil
}

// UserLabel is one entry of the id→person directory every member may read.
type UserLabel struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// ListUserLabels serves the id→label directory the owner pickers render from.
//
//	@Summary	List user labels (id, name, email) for owner pickers (admin)
//	@Tags		auth
//	@Produce	json
//	@Success	200	{array}		UserLabel
//	@Failure	403	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/auth/user-labels [get]
func (h *AuthHandler) ListUserLabels(c *gin.Context) error {
	list, err := h.users.List(c.Request.Context())
	if err != nil {
		return err
	}
	labels := make([]UserLabel, 0, len(list))
	for _, u := range list {
		labels = append(labels, UserLabel{ID: u.ID, Name: u.Name, Email: u.Email})
	}
	c.JSON(http.StatusOK, labels)
	return nil
}

// PatchUser changes an account's role or switches it off; refused when it
// would leave no enabled admin (409), on the local account, and on oneself.
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
func (h *AuthHandler) PatchUser(c *gin.Context) error {
	var req UserPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil || (req.Role == nil && req.Disabled == nil) {
		return badRequest("role and/or disabled are required")
	}
	if req.Role != nil && *req.Role != store.RoleAdmin && *req.Role != store.RoleMember {
		return badRequest("role must be admin or member")
	}
	id := c.Param("id")
	if id == store.LocalUserID {
		return badRequest("the local account is not managed")
	}
	if me, _ := server.CurrentUser(c); me.ID == id {
		return badRequest("you cannot change your own account")
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
		return conflict(err.Error())
	}
	if err != nil {
		return err
	}
	// Close what the old role opened; a still-enabled client reconnects as what
	// it is now.
	h.Conns.CloseForUser(id, "account changed")
	if req.Disabled != nil && *req.Disabled {
		if _, err := h.tokens.RevokeAllForUser(c.Request.Context(), id); err != nil {
			return err
		}
	}
	c.Status(http.StatusNoContent)
	return nil
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
func (h *AuthHandler) RevokeUserTokens(c *gin.Context) error {
	id := c.Param("id")
	if _, err := h.users.ByID(c.Request.Context(), id); err != nil {
		return err
	}
	n, err := h.tokens.RevokeAllForUser(c.Request.Context(), id)
	if err != nil {
		return err
	}
	server.SetAuditDetail(c, "tokens="+strconv.FormatInt(n, 10))
	h.Conns.CloseForUser(id, "signed out by an admin")
	c.Status(http.StatusNoContent)
	return nil
}

// requirePATMode gates the PAT endpoints: in token mode a PAT could be minted
// but never authenticate (docs/howto/workbench-auth.md).
func (h *AuthHandler) requirePATMode(c *gin.Context) (protocol.UserInfo, error) {
	if h.svc.Mode() != authn.ModeOAuth {
		return protocol.UserInfo{}, badRequest("personal access tokens require --auth oauth; token mode authenticates with the static token")
	}
	u, ok := server.CurrentUser(c)
	if !ok {
		return protocol.UserInfo{}, unauthorized("unauthorized")
	}
	return u, nil
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
func (h *AuthHandler) ListTokens(c *gin.Context) error {
	u, err := h.requirePATMode(c)
	if err != nil {
		return err
	}
	list, err := h.tokens.ListByUser(c.Request.Context(), u.ID, store.TokenKindPAT)
	if err != nil {
		return err
	}
	out := make([]protocol.PatView, 0, len(list))
	for i := range list {
		out = append(out, patView(&list[i]))
	}
	c.JSON(http.StatusOK, out)
	return nil
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
func (h *AuthHandler) CreateToken(c *gin.Context) error {
	u, err := h.requirePATMode(c)
	if err != nil {
		return err
	}
	var req struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" || req.ExpiresInDays < 0 {
		return badRequest("a token needs a name; expires_in_days is optional (0 = never)")
	}
	var expires time.Time
	if req.ExpiresInDays > 0 {
		expires = time.Now().UTC().AddDate(0, 0, req.ExpiresInDays)
	}
	secret, minted, err := h.tokens.Mint(c.Request.Context(), u.ID, store.TokenKindPAT, strings.TrimSpace(req.Name), expires)
	if err != nil {
		return err
	}
	server.SetAuditResource(c, minted.ID)
	c.JSON(http.StatusCreated, protocol.PatCreated{Token: secret, Pat: patView(minted)})
	return nil
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
func (h *AuthHandler) DeleteToken(c *gin.Context) error {
	u, err := h.requirePATMode(c)
	if err != nil {
		return err
	}
	if err := h.tokens.Revoke(c.Request.Context(), c.Param("id"), u.ID); err != nil {
		return err
	}
	// Close the user's connections; those on other credentials reconnect and carry on.
	h.Conns.CloseForUser(u.ID, "token revoked")
	c.Status(http.StatusNoContent)
	return nil
}

// Config reports how to authenticate, for the login page.
//
//	@Summary	Auth configuration
//	@Tags		auth
//	@Produce	json
//	@Success	200	{object}	protocol.AuthConfig
//	@Router		/auth/config [get]
func (h *AuthHandler) Config(c *gin.Context) error {
	c.JSON(http.StatusOK, h.svc.ConfigView())
	return nil
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
func (h *AuthHandler) Login(c *gin.Context) error {
	if h.svc.Mode() != authn.ModeToken {
		return badRequest("token login is disabled; sign in through OAuth")
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return badRequest("invalid request")
	}
	if !h.svc.StaticOK(req.Token) {
		return unauthorized("invalid token")
	}
	if u, err := h.svc.Authenticate(c.Request.Context(), req.Token); err == nil {
		server.SetAuditActor(c, u) // a login is the one line worth having on this route
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
	return nil
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
func (h *AuthHandler) Check(c *gin.Context) error {
	c.JSON(http.StatusOK, gin.H{"ok": true})
	return nil
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
func (h *AuthHandler) Me(c *gin.Context) error {
	u, ok := server.CurrentUser(c)
	if !ok {
		return unauthorized("unauthorized")
	}
	c.JSON(http.StatusOK, u)
	return nil
}

// OAuthStart redirects the browser into the named provider's authorize flow.
//
//	@Summary	Begin an OAuth login
//	@Tags		auth
//	@Param		provider	path	string	true	"Provider name: google or github"
//	@Success	302			"redirect to the provider's authorize URL"
//	@Failure	404			{object}	ErrorResponse	"unknown provider (or token mode)"
//	@Router		/auth/oauth/{provider}/start [get]
func (h *AuthHandler) OAuthStart(c *gin.Context) error {
	authURL, nonce, err := h.svc.Begin(c.Param("provider"))
	if err != nil {
		return errNotFound
	}
	// HttpOnly, Lax, and as short-lived as the pending login —
	// docs/howto/workbench-auth.md.
	name, secure := h.svc.LoginCookie()
	http.SetCookie(c.Writer, &http.Cookie{
		Name: name, Value: nonce, Path: "/", MaxAge: int(authn.LoginTTL / time.Second),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	c.Redirect(http.StatusFound, authURL)
	return nil
}

// OAuthCallback finishes a login and always redirects into the SPA: success
// carries a one-time exchange code in the fragment, failure a coarse error tag.
//
//	@Summary	OAuth provider callback
//	@Tags		auth
//	@Param		provider	path	string	true	"Provider name"
//	@Param		state		query	string	true	"Authorize round-trip state"
//	@Param		code		query	string	false	"Authorization code"
//	@Param		error		query	string	false	"The provider's error (a cancelled consent)"
//	@Success	302			"redirect into the SPA"
//	@Router		/auth/oauth/{provider}/callback [get]
func (h *AuthHandler) OAuthCallback(c *gin.Context) error {
	name, secure := h.svc.LoginCookie()
	nonce, _ := c.Cookie(name)
	http.SetCookie(c.Writer, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	redirect := h.svc.Complete(c.Request.Context(), c.Param("provider"), c.Query("state"), c.Query("code"), nonce, c.Query("error"))
	c.Redirect(http.StatusFound, redirect)
	return nil
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
func (h *AuthHandler) Exchange(c *gin.Context) error {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		return badRequest("invalid request")
	}
	token, user, ok := h.svc.Exchange(req.Code)
	if !ok {
		return unauthorized("unknown or expired code")
	}
	server.SetAuditActor(c, user) // the exchange IS the completed login
	c.JSON(http.StatusOK, protocol.AuthSession{Token: token, User: user})
	return nil
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
func (h *AuthHandler) Logout(c *gin.Context) error {
	if err := h.svc.Logout(c.Request.Context(), server.BearerToken(c)); err != nil {
		return err
	}
	if u, ok := server.CurrentUser(c); ok {
		h.Conns.CloseForUser(u.ID, "signed out")
	}
	c.Status(http.StatusNoContent)
	return nil
}
