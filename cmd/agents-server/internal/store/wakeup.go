package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// A wake-up's lifecycle. Delivered and cancelled are both terminal; they are
// distinguished only so a reader can tell "the session was told" from "nobody
// needed to be told".
const (
	WakePending   = "pending"
	WakeDelivered = "delivered"
	WakeCancelled = "cancelled"
)

// The two sources of a wake-up debt. They name what owes the turn; a source
// cancels its own debt by (kind, source_id) when the result was already seen.
const (
	WakeKindTask     = "task"
	WakeKindWorkflow = "workflow"
)

// Wakeup is one debt: a session is owed a turn carrying Payload. The debt is a
// ROW, drained when the session can take it, rather than a call that has to
// land at the wrong moment — see README invariant 32.
type Wakeup struct {
	bun.BaseModel `bun:"table:wakeups,alias:wku"`

	ID string `bun:"id,pk" json:"id"`
	// SessionID is who is owed the turn.
	SessionID string `bun:"session_id,notnull" json:"session_id"`
	// Kind and SourceID name what owes it ("task", "workflow") — the source's
	// bookkeeping handle for cancelling its own debt; the waker never reads them.
	Kind     string `bun:"kind,notnull" json:"kind"`
	SourceID string `bun:"source_id"    json:"source_id,omitempty"`
	// Inherit is the encoded run configuration the turn runs under, frozen at
	// the moment the work was ASKED for. The drain also GROUPS debts by this
	// string: one turn pays every debt with the same Inherit.
	Inherit string `bun:"inherit,nullzero" json:"-"`
	// ParentRunID is the run whose tool call started the work, so the wake-up's
	// trace nests under it instead of opening a second root.
	ParentRunID string `bun:"parent_run_id" json:"parent_run_id,omitempty"`
	// Payload is the text the turn carries.
	Payload string `bun:"payload" json:"payload"`
	// Attempt binds the debt to the try that owes it: a source retried while a
	// drain is in flight must not have the NEW attempt marked delivered by the
	// old one's launch.
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

// Owe records a debt.
func (s *WakeupStore) Owe(ctx context.Context, w *Wakeup) error {
	if _, err := s.db.NewInsert().Model(w).Exec(ctx); err != nil {
		return fmt.Errorf("recording a wake-up for session %s: %w", w.SessionID, err)
	}
	return nil
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

// Settle moves a debt out of pending, bound to the attempt that owed it. It
// reports whether this caller was the one that moved it, so a drain racing a
// cancel cannot both claim to have handled it.
func (s *WakeupStore) Settle(ctx context.Context, id, attempt, state string) (bool, error) {
	q := s.db.NewUpdate().Model((*Wakeup)(nil)).
		Set("state = ?", state).
		Where("id = ?", id).
		Where("state = ?", WakePending)
	if attempt != "" {
		q = q.Where("attempt = ?", attempt)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("settling wake-up %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, nil
}

// CancelFor drops what a source still owes — its result reached the session
// another way, or the work was cancelled and restating it would be noise. A
// non-empty attempt narrows the cancel to THAT try's debt: consuming attempt
// A's result must not cancel a retry B's fresh debt, so the caller that knows
// the attempt passes it. An empty attempt cancels every pending debt of the
// source (a workflow, whose debt has no attempt).
func (s *WakeupStore) CancelFor(ctx context.Context, kind, sourceID, attempt string) error {
	q := s.db.NewUpdate().Model((*Wakeup)(nil)).
		Set("state = ?", WakeCancelled).
		Where("kind = ?", kind).
		Where("source_id = ?", sourceID).
		Where("state = ?", WakePending)
	if attempt != "" {
		q = q.Where("attempt = ?", attempt)
	}
	if _, err := q.Exec(ctx); err != nil {
		return fmt.Errorf("cancelling wake-ups for %s %s: %w", kind, sourceID, err)
	}
	return nil
}
