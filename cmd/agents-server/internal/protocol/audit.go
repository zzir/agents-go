package protocol

import "context"

// AuditRecord is one line for the audit log: who did what, to what. Detail is
// whatever a handler chose to annotate — never a request body.
type AuditRecord struct {
	Actor    UserInfo
	Action   string
	Resource string
	Detail   string
	ClientIP string
}

// AuditFunc persists one record. The Audit middleware calls it on its own
// goroutine after the response, on a context detached from the request's.
type AuditFunc func(ctx context.Context, r AuditRecord)
