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

// ownsApproval reports whether userID owns the session the pending approval
// for toolCallID is filed on (a task's hidden session inherits its parent's
// owner, so a task's approvals are the parent's owner's to decide).
func ownsApproval(ctx context.Context, approvals *store.PendingApprovalStore, sessions *store.SessionStore, userID, toolCallID string) bool {
	pending, _, err := approvals.FindByToolCall(ctx, toolCallID)
	if err != nil {
		return false
	}
	sess, err := sessions.Get(ctx, pending.SessionID)
	return err == nil && sess.OwnerID == userID
}
