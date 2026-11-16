package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// A wake-up's lifecycle. Delivered and cancelled are both terminal,
// distinguished only for the reader.
const (
	WakePending   = "pending"
	WakeDelivered = "delivered"
	WakeCancelled = "cancelled"
)

// What owes a debt: a finished task its result, or a trigger a delivery (a
// tick, a webhook call) that is waiting to start.
const (
	WakeKindTask    = "task"
	WakeKindTrigger = "trigger"
)

// Wakeup is one debt: a session is owed a turn carrying Payload, drained
// when the session can take it — invariant 32.
type Wakeup struct {
	bun.BaseModel `bun:"table:wakeups,alias:wku"`

	ID string `bun:"id,pk,type:uuid" json:"id"`
	// SessionID is who is owed the turn; the session delete cascade removes the row.
	SessionID string `bun:"session_id,notnull,type:uuid" json:"session_id"`
	// Kind and SourceID name what owes it: a task, or a trigger.
	Kind     string `bun:"kind,notnull" json:"kind"`
	SourceID string `bun:"source_id,nullzero,type:uuid" json:"source_id,omitempty"`
	// Inherit is the encoded run configuration a task's turn runs under, frozen at the ask; empty for a trigger's.
	Inherit string `bun:"inherit,nullzero" json:"-"`
	// ParentRunID is the run whose tool call started the work; the wake-up's trace nests under it.
	ParentRunID string `bun:"parent_run_id,nullzero,type:uuid" json:"parent_run_id,omitempty"`
	// Payload is the text the turn carries; a trigger's is the delivery's body alone.
	Payload string `bun:"payload" json:"payload"`
	// Attempt binds the debt to the try that owes it: a task's run, or a trigger's delivery key, unique per trigger.
	Attempt string `bun:"attempt" json:"attempt,omitempty"`
	State   string `bun:"state,notnull" json:"state"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
}

var _ bun.BeforeAppendModelHook = (*Wakeup)(nil)

// BeforeAppendModel fills the id and timestamp on insert.
func (w *Wakeup) BeforeAppendModel(_ context.Context, query bun.Query) error {
	if _, ok := query.(*bun.InsertQuery); ok {
		if w.ID == "" {
			w.ID = NewID()
		}
		if w.CreatedAt.IsZero() {
			w.CreatedAt = time.Now().UTC()
		}
		if w.State == "" {
			w.State = WakePending
		}
	}
	return nil
}

// WakeupStore persists wake-up debts.
type WakeupStore struct {
	db *bun.DB
}

// NewWakeupStore returns a WakeupStore backed by db.
func NewWakeupStore(db *bun.DB) *WakeupStore {
	return &WakeupStore{db: db}
}

// Pending returns the session's outstanding debts, oldest first — one drain
// pays them all, so a session that piled up three results is woken once.
func (s *WakeupStore) Pending(ctx context.Context, sessionID string) ([]Wakeup, error) {
	var out []Wakeup
	if err := s.db.NewSelect().Model(&out).
		Where("session_id = ?", sessionID).
		Where("state = ?", WakePending).
		OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing wake-ups for session %s: %w", sessionID, err)
	}
	return out, nil
}

// PendingSessions returns every session owed something — the restart sweep.
func (s *WakeupStore) PendingSessions(ctx context.Context) ([]string, error) {
	var out []string
	if err := s.db.NewSelect().Model((*Wakeup)(nil)).
		ColumnExpr("DISTINCT session_id").
		Where("state = ?", WakePending).
		Scan(ctx, &out); err != nil {
		return nil, fmt.Errorf("listing sessions owed a wake-up: %w", err)
	}
	return out, nil
}

// Settle moves a debt out of pending, bound to the attempt that owed it,
// reporting whether this caller was the one that moved it.
func (s *WakeupStore) Settle(ctx context.Context, id, attempt, state string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*Wakeup)(nil)).
		Set("state = ?", state).
		Where("id = ?", id).
		Where("attempt = ?", attempt).
		Where("state = ?", WakePending).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("settling wake-up %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, nil
}

// Owe records a trigger's delivery as a debt, reporting false when the
// trigger already holds one under the same attempt: a replayed delivery.
func (s *WakeupStore) Owe(ctx context.Context, w *Wakeup) (bool, error) {
	res, err := s.db.NewInsert().Model(w).On("CONFLICT DO NOTHING").Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("recording a delivery of trigger %s: %w", w.SourceID, err)
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, nil
}

// PendingFor returns what one source still owes, oldest first — a trigger's backlog.
func (s *WakeupStore) PendingFor(ctx context.Context, kind, sourceID string) ([]Wakeup, error) {
	var out []Wakeup
	if err := s.db.NewSelect().Model(&out).
		Where("kind = ?", kind).
		Where("source_id = ?", sourceID).
		Where("state = ?", WakePending).
		OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing what %s %s owes: %w", kind, sourceID, err)
	}
	return out, nil
}

// Reopen returns a debt a Settle marked delivered to pending: the start that
// claim was for did not happen.
func (s *WakeupStore) Reopen(ctx context.Context, id string) error {
	if _, err := s.db.NewUpdate().Model((*Wakeup)(nil)).
		Set("state = ?", WakePending).
		Where("id = ?", id).
		Where("state = ?", WakeDelivered).
		Exec(ctx); err != nil {
		return fmt.Errorf("reopening wake-up %s: %w", id, err)
	}
	return nil
}

// Forget removes a debt as if it was never owed, so the same delivery may be
// sent again.
func (s *WakeupStore) Forget(ctx context.Context, id string) error {
	if _, err := s.db.NewDelete().Model((*Wakeup)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("forgetting wake-up %s: %w", id, err)
	}
	return nil
}

// CancelFor drops what a source still owes for ONE attempt (its result
// reached the session another way); a retry's fresh debt is untouched.
func (s *WakeupStore) CancelFor(ctx context.Context, kind, sourceID, attempt string) error {
	if _, err := s.db.NewUpdate().Model((*Wakeup)(nil)).
		Set("state = ?", WakeCancelled).
		Where("kind = ?", kind).
		Where("source_id = ?", sourceID).
		Where("attempt = ?", attempt).
		Where("state = ?", WakePending).
		Exec(ctx); err != nil {
		return fmt.Errorf("cancelling wake-ups for %s %s: %w", kind, sourceID, err)
	}
	return nil
}

// DeleteSettledBefore removes delivered and cancelled wake-ups created before
// cutoff.
func (s *WakeupStore) DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := deleteInBatches(ctx, s.db, (*Wakeup)(nil), func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("state IN (?, ?) AND created_at < ?", WakeDelivered, WakeCancelled, cutoff)
	})
	if err != nil {
		return n, fmt.Errorf("pruning settled wake-ups: %w", err)
	}
	return n, nil
}
