package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Authorization, in two rules (README "Ownership and roles"):
//
//   - Shared configuration — agents, providers, MCP servers, sandboxes,
//     settings, skills, workflows, guardrails, memories, routes — is readable
//     by every member and written only by admins. Every such write is a
//     change to what runs on the host or whose credentials are spent.
//   - A session's content belongs to its owner alone; an admin may list, stop
//     and delete one (management), never read it.

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
		if _, ok := requireOwnedSession(c, d.Sessions, c.Param("id")); !ok {
			c.Abort()
			return
		}
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
