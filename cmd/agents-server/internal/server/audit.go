package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// AuditRecord is one line for the audit log: who did what, to what. Detail is
// whatever a handler chose to annotate — never a request body.
type AuditRecord struct {
	Actor    protocol.UserInfo
	Action   string
	Resource string
	Detail   string
	ClientIP string
}

// AuditFunc persists one record. Audit calls it on its own goroutine, after
// the response, on a context detached from the request's cancellation — a
// slow write never delays a reply.
type AuditFunc func(ctx context.Context, r AuditRecord)

const (
	auditDetailKey   = "agents.audit.detail"
	auditResourceKey = "agents.audit.resource"
	auditActorKey    = "agents.audit.actor"
)

// SetAuditDetail annotates the request's audit line (an approval's scope, a
// role) — the semantic a route pattern alone cannot say.
func SetAuditDetail(c *gin.Context, detail string) { c.Set(auditDetailKey, detail) }

// SetAuditResource names the resource a request created, where no path
// parameter carries it.
func SetAuditResource(c *gin.Context, id string) { c.Set(auditResourceKey, id) }

// SetAuditActor names the caller on a route that establishes who they are
// (login, the OAuth exchange) — auth-exempt, so TokenAuth attached nobody.
func SetAuditActor(c *gin.Context, u protocol.UserInfo) { c.Set(auditActorKey, u) }

// Audit records every successful mutating API request after it completes:
// "METHOD /route/pattern" with the first path parameter as the resource. A
// request nobody authenticated (an auth-exempt route that did not name its
// actor) leaves no line — there is no one to attribute it to.
func Audit(record AuditFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return
		}
		if c.Writer.Status() >= 300 || !strings.HasPrefix(c.Request.URL.Path, APIPrefix) {
			return
		}
		actor, ok := CurrentUser(c)
		if !ok {
			v, found := c.Get(auditActorKey)
			if actor, ok = v.(protocol.UserInfo); !found || !ok {
				return
			}
		}
		r := AuditRecord{
			Actor:    actor,
			Action:   c.Request.Method + " " + strings.TrimPrefix(c.FullPath(), APIPrefix),
			ClientIP: c.ClientIP(),
		}
		if len(c.Params) > 0 {
			r.Resource = c.Params[0].Value
		}
		if v, found := c.Get(auditResourceKey); found {
			r.Resource, _ = v.(string)
		}
		if v, found := c.Get(auditDetailKey); found {
			r.Detail, _ = v.(string)
		}
		go record(context.WithoutCancel(c.Request.Context()), r)
	}
}
