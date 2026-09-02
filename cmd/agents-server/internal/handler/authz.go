package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Authorization gates: session content is owner-only, scoped configuration
// gates per row, host configuration is read-everyone/write-admin —
// decisions §5.29.

// requireAdmin answers 403 unless the caller is an admin; false means the
// response is written and the handler must return.
func requireAdmin(c *gin.Context) bool {
	u, ok := server.CurrentUser(c)
	if !ok || u.Role != store.RoleAdmin {
		c.JSON(http.StatusForbidden, protocol.NewErrorResponse(protocol.CodeForbidden, "admin role required"))
		return false
	}
	return true
}

// adminOnly is requireAdmin as middleware, for the write routes of shared
// configuration.
func adminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			c.Abort()
			return
		}
		c.Next()
	}
}

// ownsSession reports whether the caller owns sess. In token mode the one
// local user owns everything, so every check passes.
func ownsSession(c *gin.Context, sess *store.Session) bool {
	u, ok := server.CurrentUser(c)
	return ok && u.ID == sess.OwnerID
}

// requireOwnedSession loads the session and checks the caller owns it. A
// session someone else owns answers 404, the same as one that does not
// exist: ownership is not an oracle for existence.
func requireOwnedSession(c *gin.Context, sessions *store.SessionStore, id string) (*store.Session, bool) {
	sess, err := sessions.Get(c.Request.Context(), id)
	if err != nil {
		storeError(c, err)
		return nil, false
	}
	if !ownsSession(c, sess) {
		notFound(c)
		return nil, false
	}
	return sess, true
}

// sessionKey is where sessionGate parks the row it loaded, so the handler
// behind it reads the session once, not twice.
const sessionKey = "agents.session"

// gatedSession returns the session sessionGate loaded for this request, or
// loads it when no gate ran (a handler mounted without one).
func gatedSession(c *gin.Context, sessions *store.SessionStore) (*store.Session, error) {
	if v, ok := c.Get(sessionKey); ok {
		if sess, ok := v.(*store.Session); ok && sess.ID == c.Param("id") {
			return sess, nil
		}
	}
	return sessions.Get(c.Request.Context(), c.Param("id"))
}

// requireRunOwner checks the caller owns the session a live run belongs to
// (hub record), answering 404 for a foreign or unknown run.
func requireRunOwner(c *gin.Context, info bridge.RunInfo, ok bool) bool {
	u, authed := server.CurrentUser(c)
	if !ok || !authed || u.ID != info.OwnerID {
		notFound(c)
		return false
	}
	return true
}

// ownsApproval returns the pending tool call toolCallID when userID owns the
// session its approval is filed on (a task's hidden session inherits its
// parent's owner, so a task's approvals are the parent's owner's to decide).
func ownsApproval(ctx context.Context, approvals *store.PendingApprovalStore, sessions *store.SessionStore, userID, toolCallID string) (*store.PendingToolCall, bool) {
	pending, call, err := approvals.FindByToolCall(ctx, toolCallID)
	if err != nil {
		return nil, false
	}
	sess, err := sessions.Get(ctx, pending.SessionID)
	if err != nil || sess.OwnerID != userID {
		return nil, false
	}
	return call, true
}

// approvalKey is where approvalGate parks the pending call for the handler.
const approvalKey = "agents.approval"

// pendingCall returns the call approvalGate loaded, if it ran.
func pendingCall(c *gin.Context) *store.PendingToolCall {
	v, _ := c.Get(approvalKey)
	p, _ := v.(*store.PendingToolCall)
	return p
}

// AuthzDeps are the stores the route gates resolve ownership through.
type AuthzDeps struct {
	Sessions  *store.SessionStore
	Tasks     *store.TaskStore
	Approvals *store.PendingApprovalStore
	Triggers  *store.TriggerStore
	Hub       *bridge.RunHub
}

// sessionGate gates /sessions/:id/* on owning the session.
func (d AuthzDeps) sessionGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess, ok := requireOwnedSession(c, d.Sessions, c.Param("id"))
		if !ok {
			c.Abort()
			return
		}
		c.Set(sessionKey, sess)
		c.Next()
	}
}

// runGate gates /runs/:id/* on owning the live run's session.
func (d AuthzDeps) runGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		info, ok := d.Hub.Info(c.Param("id"))
		if !requireRunOwner(c, info, ok) {
			c.Abort()
			return
		}
		c.Next()
	}
}

// taskGate gates /tasks/:id/* on owning the task's parent session.
func (d AuthzDeps) taskGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		t, err := d.Tasks.Get(c.Request.Context(), c.Param("id"))
		if err != nil {
			storeError(c, err)
			c.Abort()
			return
		}
		if _, ok := requireOwnedSession(c, d.Sessions, t.ParentSessionID); !ok {
			c.Abort()
			return
		}
		c.Next()
	}
}

// approvalGate gates /approvals/:tool_call_id/* on owning the session the
// approval is filed on.
func (d AuthzDeps) approvalGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, _ := server.CurrentUser(c)
		pending, ok := ownsApproval(c.Request.Context(), d.Approvals, d.Sessions, u.ID, c.Param("tool_call_id"))
		if !ok {
			notFound(c)
			c.Abort()
			return
		}
		c.Set(approvalKey, pending)
		c.Next()
	}
}

// triggerGate gates /triggers/:id/* on owning the session the trigger fires
// into.
func (d AuthzDeps) triggerGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		t, err := d.Triggers.Get(c.Request.Context(), c.Param("id"))
		if err != nil {
			storeError(c, err)
			c.Abort()
			return
		}
		if _, ok := requireOwnedSession(c, d.Sessions, t.SessionID); !ok {
			c.Abort()
			return
		}
		c.Next()
	}
}

// The scoped-configuration gates (decisions §5.29). scopeOf reads a row's
// (scope, owner) pair; rowGate is one of visibleRow, editableRow and
// deletableRow.

// listVisible answers the rows the caller may see; false means the response
// is written.
func listVisible[T any](c *gin.Context, s *store.CrudStore[T]) ([]T, bool) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return nil, false
	}
	rows, err := store.ListVisibleOf(c.Request.Context(), s, ownerID, admin)
	if err != nil {
		internalError(c, err)
		return nil, false
	}
	return rows, true
}

// gatedRow loads the row the id path parameter names and runs gate on its
// scope pair; false means the response is written.
func gatedRow[T any](c *gin.Context, s *store.CrudStore[T], scopeOf func(*T) (string, string), gate func(*gin.Context, string, string) bool) (*T, bool) {
	row, err := s.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return nil, false
	}
	if scope, owner := scopeOf(row); !gate(c, scope, owner) {
		return nil, false
	}
	return row, true
}

// deleteOwned deletes the row the id path parameter names, with the owner
// deletableRow authorized against as the delete's predicate (409 when it
// moved since). False means the response is written.
func deleteOwned[T any](c *gin.Context, s *store.CrudStore[T], scopeOf func(*T) (string, string)) bool {
	row, ok := gatedRow(c, s, scopeOf, deletableRow)
	if !ok {
		return false
	}
	_, owner := scopeOf(row)
	if err := store.DeleteOwnedBy(c.Request.Context(), s, c.Param("id"), owner); err != nil {
		saveError(c, err) // moved since the check -> 409
		return false
	}
	return true
}

// stampCreateScope applies the caller to a new scoped row: an explicit
// global claim needs the admin role, anything else lands private and owned.
// False means the response is written.
func stampCreateScope(c *gin.Context, scope, ownerID *string) bool {
	u, ok := server.CurrentUser(c)
	if !ok {
		notFound(c)
		return false
	}
	if *scope != "" && *scope != store.ScopeGlobal && *scope != store.ScopePrivate {
		badRequest(c, `scope must be "global" or "private"`)
		return false
	}
	if *scope == store.ScopeGlobal && u.Role != store.RoleAdmin {
		abortError(c, http.StatusForbidden, protocol.CodeForbidden, "admin role required to create global configuration")
		return false
	}
	*scope, *ownerID = store.NormalizeScope(*scope), u.ID
	return true
}

// scopeChangeAllowed authorizes a scope flip on a row the caller must first
// be able to see (404 otherwise): promoting — publishing to every member —
// is an admin's act; demoting is the admin's or the author's (the row
// returns to its owner). False means the response is written.
func scopeChangeAllowed(c *gin.Context, target, rowScope, rowOwner string) bool {
	if !visibleRow(c, rowScope, rowOwner) {
		return false
	}
	u, _ := server.CurrentUser(c)
	admin := u.Role == store.RoleAdmin
	if target == store.ScopeGlobal && !admin {
		abortError(c, http.StatusForbidden, protocol.CodeForbidden, "admin role required to publish configuration")
		return false
	}
	if target == store.ScopePrivate && !admin && rowOwner != u.ID {
		abortError(c, http.StatusForbidden, protocol.CodeForbidden, "only an admin or the owner may unpublish this configuration")
		return false
	}
	return true
}

// setScopePlain is the /scope POST body shared by entities with no extra
// validation (MCP servers): bind, authorize, refuse the same scope, flip.
// Entities with more to check (providers' demote guard, agents' and
// workflows' reference validation, skills' repo grouping) keep their own
// handlers.
func setScopePlain[T any](c *gin.Context, s *store.CrudStore[T], kind string, scopeOf func(*T) (scope, owner string)) {
	scope, ok := bindScope(c)
	if !ok {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	row, err := s.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	cur, owner := scopeOf(row)
	if !scopeChangeAllowed(c, scope, cur, owner) {
		return
	}
	if sameScope(c, kind, cur, scope) {
		return
	}
	if err := store.SetScopeOf(ctx, s, id, scope, owner); err != nil {
		saveError(c, err) // name collision in the target scope -> 409
		return
	}
	c.Status(http.StatusNoContent)
}

// setOwnerPlain is the /owner PUT body shared by the scoped entities (the
// route carries the admin gate): the row moves to another account; scope
// stays put. A name already taken in the target owner's private namespace
// answers 409.
func setOwnerPlain[T any](c *gin.Context, s *store.CrudStore[T]) {
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		badRequest(c, "user_id is required")
		return
	}
	if err := store.SetOwnerOf(c.Request.Context(), s, c.Param("id"), req.UserID); err != nil {
		if errors.Is(err, store.ErrNoSuchUser) {
			badRequest(c, "no such user")
			return
		}
		saveError(c, err) // name collision in the target owner's namespace -> 409
		return
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
}

// ownershipGuard folds the authorization check INTO the write transaction:
// the returned hook runs against the LOCKED row, so a transfer or scope flip
// that landed since editableRow ran turns the write into a 409 instead of an
// edit by somebody who may no longer make it (decisions §5.29). next is the
// entity's own prepare hook, run after the check.
func ownershipGuard[T any](scope, owner string, scopeOf func(*T) (string, string), next func(*T) error) func(*T) error {
	return func(prev *T) error {
		if s, o := scopeOf(prev); s != scope || o != owner {
			return store.ErrOwnershipChanged
		}
		if next != nil {
			return next(prev)
		}
		return nil
	}
}

// callerSees reports whether the caller may see a row — the predicate behind
// treating foreign private rows as absent in validation messages too.
func callerSees(c *gin.Context, scope, rowOwner string) bool {
	u, ok := server.CurrentUser(c)
	return ok && store.Visible(scope, rowOwner, u.ID, u.Role == store.RoleAdmin)
}

// visibleRow 404s a row the caller may not see — a foreign private row reads
// as absent, never as forbidden (ownership is not an oracle for existence).
func visibleRow(c *gin.Context, scope, rowOwner string) bool {
	u, ok := server.CurrentUser(c)
	if !ok || !store.Visible(scope, rowOwner, u.ID, u.Role == store.RoleAdmin) {
		notFound(c)
		return false
	}
	return true
}

// editableRow gates an UPDATE: the owner edits what they created — private
// or published — and an admin additionally edits any global row. An admin
// does NOT edit a member's private row — management is delete, scope change
// and transfer, not authorship. Invisibility answers 404 first, a
// visible-but-not-yours row 403.
func editableRow(c *gin.Context, scope, rowOwner string) bool {
	if !visibleRow(c, scope, rowOwner) {
		return false
	}
	u, _ := server.CurrentUser(c)
	if rowOwner == u.ID {
		return true
	}
	if scope == store.ScopeGlobal {
		if u.Role == store.RoleAdmin {
			return true
		}
		abortError(c, http.StatusForbidden, protocol.CodeForbidden, "only an admin or the owner may modify global configuration")
		return false
	}
	abortError(c, http.StatusForbidden, protocol.CodeForbidden, "this configuration belongs to another user")
	return false
}

// deletableRow gates a DELETE: everything editableRow allows, plus an admin
// removing any row (management).
func deletableRow(c *gin.Context, scope, rowOwner string) bool {
	if !visibleRow(c, scope, rowOwner) {
		return false
	}
	u, _ := server.CurrentUser(c)
	if u.Role == store.RoleAdmin {
		return true
	}
	return editableRow(c, scope, rowOwner)
}

// callerScope answers the caller's (id, admin) pair for visibility-filtered
// listings.
func callerScope(c *gin.Context) (string, bool, bool) {
	u, ok := server.CurrentUser(c)
	if !ok {
		notFound(c)
		return "", false, false
	}
	return u.ID, u.Role == store.RoleAdmin, true
}
