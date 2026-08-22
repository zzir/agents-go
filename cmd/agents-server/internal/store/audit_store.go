package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// AuditEvent is one line of "who did what, to what, when": every successful
// mutating API request, plus the acts that bypass REST (WS run starts and
// approval decisions, terminal opens). Detail never carries a request body or
// a secret — a scope, a role, a sandbox id. Retention is the process's
// --audit-retention-days, never an API setting: the log of configuration
// changes must not be shortened through the API it records.
type AuditEvent struct {
	bun.BaseModel `bun:"table:audit_events,alias:ae"`

	ID string `bun:"id,pk,type:uuid" json:"id"`
	// ActorID/ActorEmail identify the caller; the email is a snapshot so the
	// line stays readable after the account is gone.
	ActorID    string `bun:"actor_id,notnull,type:uuid" json:"actor_id"`
	ActorEmail string `bun:"actor_email,nullzero" json:"actor_email,omitempty"`
	// Action is "METHOD /route/pattern" for REST, or a dotted name for the
	// explicit events (ws.run.create, ws.approval, terminal.open).
	Action   string `bun:"action,notnull"     json:"action"`
	Resource string `bun:"resource,nullzero"  json:"resource,omitempty"`
	Detail   string `bun:"detail,nullzero"    json:"detail,omitempty"`
	ClientIP string `bun:"client_ip,nullzero" json:"client_ip,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}

// AuditStore appends and reads the audit log.
type AuditStore struct{ db *bun.DB }

// NewAuditStore returns an AuditStore backed by db.
func NewAuditStore(db *bun.DB) *AuditStore { return &AuditStore{db: db} }

// Record appends one event, stamping id and time.
func (s *AuditStore) Record(ctx context.Context, e *AuditEvent) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if _, err := s.db.NewInsert().Model(e).Exec(ctx); err != nil {
		return fmt.Errorf("recording audit event: %w", err)
	}
	return nil
}

// ListRecent returns up to limit events newest first, those before the row
// beforeID when it is set — a cursor for paging backwards. The id is a
// UUIDv7, so it orders like created_at and, unlike it, never ties.
func (s *AuditStore) ListRecent(ctx context.Context, limit int, beforeID string) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	var out []AuditEvent
	q := s.db.NewSelect().Model(&out)
	if beforeID != "" {
		q = q.Where("id < ?", beforeID)
	}
	if err := q.OrderExpr("id DESC").Limit(limit).Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing audit events: %w", err)
	}
	return out, nil
}

// DeleteOlderThan prunes events created before cutoff.
func (s *AuditStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := deleteInBatches(ctx, s.db, (*AuditEvent)(nil), func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("created_at < ?", cutoff)
	})
	if err != nil {
		return n, fmt.Errorf("pruning audit events: %w", err)
	}
	return n, nil
}
