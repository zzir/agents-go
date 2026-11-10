package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Authorization gates: session content is owner-only, scoped configuration
// gates per row, host configuration is read-everyone/write-admin — decisions §5.29.

// requireAdmin is a 403 unless the caller is an admin. Wrapped, it is the gate
// on the write routes of shared configuration.
func requireAdmin(c *gin.Context) error {
	u, ok := server.CurrentUser(c)
	if !ok || u.Role != store.RoleAdmin {
		return forbidden("admin role required")
	}
	return nil
}

// ownsSession reports whether the caller owns sess. In token mode the one
// local user owns everything, so every check passes.
func ownsSession(c *gin.Context, sess *store.Session) bool {
	u, ok := server.CurrentUser(c)
	return ok && u.ID == sess.OwnerID
}

// requireOwnedSession loads the session and checks the caller owns it; a
// foreign session is 404, the same as an absent one.
func requireOwnedSession(c *gin.Context, sessions *store.SessionStore, id string) (*store.Session, error) {
	sess, err := sessions.Get(c.Request.Context(), id)
	if err != nil {
		return nil, err
	}
	if !ownsSession(c, sess) {
		return nil, errNotFound
	}
	return sess, nil
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
// (hub record); a foreign or unknown run is 404.
func requireRunOwner(c *gin.Context, info bridge.RunInfo, ok bool) error {
	u, authed := server.CurrentUser(c)
	if !ok || !authed || u.ID != info.OwnerID {
		return errNotFound
	}
	return nil
}

// ownsApproval returns the pending tool call toolCallID when userID owns the
// session it is filed on (a task's hidden session inherits the parent's owner);
// foreign is ErrNotFound.
func ownsApproval(ctx context.Context, approvals *store.PendingApprovalStore, sessions *store.SessionStore, userID, toolCallID string) (*store.PendingToolCall, error) {
	pending, call, err := approvals.FindByToolCall(ctx, toolCallID)
	if err != nil {
		return nil, err
	}
	sess, err := sessions.Get(ctx, pending.SessionID)
	if err != nil {
		return nil, err
	}
	if sess.OwnerID != userID {
		return nil, store.ErrNotFound
	}
	return call, nil
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
	return wrap(func(c *gin.Context) error {
		sess, err := requireOwnedSession(c, d.Sessions, c.Param("id"))
		if err != nil {
			return err
		}
		c.Set(sessionKey, sess)
		return nil
	})
}

// runGate gates /runs/:id/* on owning the live run's session.
func (d AuthzDeps) runGate() gin.HandlerFunc {
	return wrap(func(c *gin.Context) error {
		info, ok := d.Hub.Info(c.Param("id"))
		return requireRunOwner(c, info, ok)
	})
}

// taskGate gates /tasks/:id/* on owning the task's parent session.
func (d AuthzDeps) taskGate() gin.HandlerFunc {
	return wrap(func(c *gin.Context) error {
		t, err := d.Tasks.Get(c.Request.Context(), c.Param("id"))
		if err != nil {
			return err
		}
		_, err = requireOwnedSession(c, d.Sessions, t.ParentSessionID)
		return err
	})
}

// approvalGate gates /approvals/:tool_call_id/* on owning the session the
// approval is filed on.
func (d AuthzDeps) approvalGate() gin.HandlerFunc {
	return wrap(func(c *gin.Context) error {
		u, _ := server.CurrentUser(c)
		pending, err := ownsApproval(c.Request.Context(), d.Approvals, d.Sessions, u.ID, c.Param("tool_call_id"))
		if err != nil {
			return err
		}
		c.Set(approvalKey, pending)
		return nil
	})
}

// triggerKey is where triggerGate parks the row for the handler.
const triggerKey = "agents.trigger"

// gatedTrigger returns the trigger triggerGate loaded for this request.
func gatedTrigger(c *gin.Context) *store.Trigger {
	return c.MustGet(triggerKey).(*store.Trigger)
}

// triggerGate gates /triggers/:id/* on owning the session the trigger fires
// into.
func (d AuthzDeps) triggerGate() gin.HandlerFunc {
	return wrap(func(c *gin.Context) error {
		t, err := d.Triggers.Get(c.Request.Context(), c.Param("id"))
		if err != nil {
			return err
		}
		if _, err := requireOwnedSession(c, d.Sessions, t.SessionID); err != nil {
			return err
		}
		c.Set(triggerKey, t)
		return nil
	})
}

// The scoped-configuration gates (decisions §5.29). scopeOf reads a row's
// (scope, owner) pair; rowGate is one of visibleRow, editableRow and deletableRow.

// listVisible returns the rows the caller may see.
func listVisible[T any](c *gin.Context, s *store.CrudStore[T]) ([]T, error) {
	ownerID, admin, err := callerScope(c)
	if err != nil {
		return nil, err
	}
	rows, err := store.ListVisibleOf(c.Request.Context(), s, ownerID, admin)
	if err != nil {
		return nil, err
	}
	return nonNilList(rows), nil
}

// nonNilList turns a nil slice into an empty one so a list endpoint answers
// [] for an empty collection, never null — see protocol.md.
func nonNilList[T any](rows []T) []T {
	if rows == nil {
		return []T{}
	}
	return rows
}

// gatedRow loads the row the id path parameter names and runs gate on its
// scope pair.
func gatedRow[T any](c *gin.Context, s *store.CrudStore[T], scopeOf func(*T) (string, string), gate func(*gin.Context, string, string) error) (*T, error) {
	row, err := s.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		return nil, err
	}
	scope, owner := scopeOf(row)
	if err := gate(c, scope, owner); err != nil {
		return nil, err
	}
	return row, nil
}

// deleteOwned deletes the row the id path parameter names through del, the
// store's own delete, with the owner deletableRow saw as the predicate (409
// when it moved).
func deleteOwned[T any](c *gin.Context, s *store.CrudStore[T], scopeOf func(*T) (string, string), del func(ctx context.Context, id, owner string) error) error {
	row, err := gatedRow(c, s, scopeOf, deletableRow)
	if err != nil {
		return err
	}
	_, owner := scopeOf(row)
	if err := del(c.Request.Context(), c.Param("id"), owner); err != nil {
		return err // moved since the check -> 409
	}
	return nil
}

// stampCreateScope applies the caller to a new scoped row: a global claim needs
// the admin role, anything else lands private.
func stampCreateScope(c *gin.Context, scope, ownerID *string) error {
	u, ok := server.CurrentUser(c)
	if !ok {
		return errNotFound
	}
	if *scope != "" && *scope != store.ScopeGlobal && *scope != store.ScopePrivate {
		return badRequest(`scope must be "global" or "private"`)
	}
	if *scope == store.ScopeGlobal && u.Role != store.RoleAdmin {
		return forbidden("admin role required to create global configuration")
	}
	*scope, *ownerID = store.NormalizeScope(*scope), u.ID
	return nil
}

// scopeChangeAllowed authorizes a scope flip on a row the caller can see (404
// otherwise): promote is the admin's, demote the admin's or the author's.
func scopeChangeAllowed(c *gin.Context, target, rowScope, rowOwner string) error {
	if err := visibleRow(c, rowScope, rowOwner); err != nil {
		return err
	}
	u, _ := server.CurrentUser(c)
	admin := u.Role == store.RoleAdmin
	if target == store.ScopeGlobal && !admin {
		return forbidden("admin role required to publish configuration")
	}
	if target == store.ScopePrivate && !admin && rowOwner != u.ID {
		return forbidden("only an admin or the owner may unpublish this configuration")
	}
	return nil
}

// setScope is the /scope POST body of every scoped entity: bind, authorize,
// refuse the same scope, run the entity's own check on the row as it would
// stand (nil for none), flip.
func setScope[T any](c *gin.Context, s *store.CrudStore[T], kind string, scopeOf func(*T) (scope, owner string), check func(row *T, target string) error, set func(ctx context.Context, id, scope, expectOwner string) error) error {
	scope, err := bindScope(c)
	if err != nil {
		return err
	}
	ctx, id := c.Request.Context(), c.Param("id")
	row, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	cur, owner := scopeOf(row)
	if err := scopeChangeAllowed(c, scope, cur, owner); err != nil {
		return err
	}
	if err := sameScope(kind, cur, scope); err != nil {
		return err
	}
	if check != nil {
		if err := check(row, scope); err != nil {
			return err
		}
	}
	if err := set(ctx, id, scope, owner); err != nil {
		return err // name collision in the target scope -> 409
	}
	server.SetAuditDetail(c, "scope="+scope)
	c.Status(http.StatusNoContent)
	return nil
}

// setOwner is the /owner PUT body of every transferable row (the route carries
// the admin gate): bind the new owner, move the row through set, record it.
func setOwner(c *gin.Context, set func(ctx context.Context, id, ownerID string) error) error {
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		return badRequest("user_id is required")
	}
	if err := set(c.Request.Context(), c.Param("id"), req.UserID); err != nil {
		return err // no such user -> 400, a taken name in the owner's namespace -> 409
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
	return nil
}

// ownershipGuard re-checks (scope, owner) against the LOCKED row inside the
// write transaction (409 when it moved — decisions §5.29); next runs after it.
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

// refRefusal says why a holder may not reference the row (kind, name), or ""
// when it may. hidden is a row the caller cannot see either: it reads as
// absent, never as refused — decisions §5.29.
func refRefusal(c *gin.Context, kind, name, refScope, refOwner, holderScope, holderOwner string) (reason string, hidden bool) {
	if store.RefVisible(refScope, refOwner, holderScope, holderOwner) {
		return "", false
	}
	if !callerSees(c, refScope, refOwner) {
		return "", true
	}
	if holderScope == store.ScopeGlobal {
		return "a global agent can only reference global configuration; " + kind + " " + name + " is private", false
	}
	return kind + " " + name + " belongs to another user", false
}

// runnableRow gates the RUN surface of an agent (a bound session, its tool
// listing, a playground call): an admin does not run a member's private row (403).
func runnableRow(c *gin.Context, scope, rowOwner string) error {
	u, ok := server.CurrentUser(c)
	if ok && u.Role == store.RoleAdmin && !store.Visible(scope, rowOwner, u.ID, false) {
		return forbidden("an admin does not run a member's private agent")
	}
	return nil
}

// visibleRow 404s a row the caller may not see — a foreign private row reads
// as absent, never as forbidden (ownership is not an oracle for existence).
func visibleRow(c *gin.Context, scope, rowOwner string) error {
	u, ok := server.CurrentUser(c)
	if !ok || !store.Visible(scope, rowOwner, u.ID, u.Role == store.RoleAdmin) {
		return errNotFound
	}
	return nil
}

// editableRow gates an UPDATE: the owner, or an admin on a global row — never
// an admin on a member's private row (decisions §5.29). 404 if invisible, else 403.
func editableRow(c *gin.Context, scope, rowOwner string) error {
	if err := visibleRow(c, scope, rowOwner); err != nil {
		return err
	}
	u, _ := server.CurrentUser(c)
	if rowOwner == u.ID {
		return nil
	}
	if scope == store.ScopeGlobal {
		if u.Role == store.RoleAdmin {
			return nil
		}
		return forbidden("only an admin or the owner may modify global configuration")
	}
	return forbidden("this configuration belongs to another user")
}

// deletableRow gates a DELETE: everything editableRow allows, plus an admin
// removing any row (management).
func deletableRow(c *gin.Context, scope, rowOwner string) error {
	if err := visibleRow(c, scope, rowOwner); err != nil {
		return err
	}
	u, _ := server.CurrentUser(c)
	if u.Role == store.RoleAdmin {
		return nil
	}
	return editableRow(c, scope, rowOwner)
}

// callerScope answers the caller's (id, admin) pair for visibility-filtered
// listings.
func callerScope(c *gin.Context) (string, bool, error) {
	u, ok := server.CurrentUser(c)
	if !ok {
		return "", false, errNotFound
	}
	return u.ID, u.Role == store.RoleAdmin, nil
}
