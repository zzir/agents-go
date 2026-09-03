package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// PendingApprovalStore persists runs paused for human-in-the-loop approval.
type PendingApprovalStore struct {
	db *bun.DB
}

// NewPendingApprovalStore returns a store backed by db.
func NewPendingApprovalStore(db *bun.DB) *PendingApprovalStore {
	return &PendingApprovalStore{db: db}
}

// Save inserts or replaces the pending approval for its run id.
func (s *PendingApprovalStore) Save(ctx context.Context, p *PendingApproval) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if _, err := s.db.NewInsert().Model(p).
		On("CONFLICT (run_id) DO UPDATE").
		Set("state = EXCLUDED.state").
		Set("tool_calls = EXCLUDED.tool_calls").
		Exec(ctx); err != nil {
		return fmt.Errorf("saving pending approval %s: %w", p.RunID, err)
	}
	return nil
}

// Get returns the pending approval for runID, or an ErrNotFound-wrapping error.
func (s *PendingApprovalStore) Get(ctx context.Context, runID string) (*PendingApproval, error) {
	p := new(PendingApproval)
	if err := s.db.NewSelect().Model(p).Where("run_id = ?", runID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting pending approval %s: %w", runID, err)
	}
	return p, nil
}

// ListBySession returns the pending approvals for a session, oldest first.
func (s *PendingApprovalStore) ListBySession(ctx context.Context, sessionID string) ([]PendingApproval, error) {
	var out []PendingApproval
	if err := s.db.NewSelect().Model(&out).
		Where("session_id = ?", sessionID).
		OrderExpr("created_at ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing pending approvals for session %s: %w", sessionID, err)
	}
	return out, nil
}

// TaskApproval is a pending approval inside a background task's child
// session, tagged with the owning task.
type TaskApproval struct {
	PendingApproval
	TaskID    string `bun:"task_id"    json:"task_id"`
	TaskLabel string `bun:"task_label" json:"task_label,omitempty"`
}

// ListByParentTasks returns the pending approvals of every background task
// spawned from the given chat session — one join instead of a per-task query.
func (s *PendingApprovalStore) ListByParentTasks(ctx context.Context, parentSessionID string) ([]TaskApproval, error) {
	var out []TaskApproval
	if err := s.db.NewSelect().Model((*PendingApproval)(nil)).
		ColumnExpr("pa.*").
		ColumnExpr("t.id AS task_id").
		ColumnExpr("t.label AS task_label").
		Join("JOIN tasks AS t ON t.child_session_id = pa.session_id").
		Where("t.parent_session_id = ?", parentSessionID).
		OrderExpr("pa.created_at ASC").
		Scan(ctx, &out); err != nil {
		return nil, fmt.Errorf("listing task approvals for session %s: %w", parentSessionID, err)
	}
	return out, nil
}

// List returns every pending approval, oldest first. Recovery after a
// restart is lazy: approvals resume from these rows on the next decision.
func (s *PendingApprovalStore) List(ctx context.Context) ([]PendingApproval, error) {
	var out []PendingApproval
	if err := s.db.NewSelect().Model(&out).
		OrderExpr("created_at ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing pending approvals: %w", err)
	}
	return out, nil
}

// FindByToolCall returns the pending approval whose tool_calls contains the
// given tool call id; matching is done in Go.
func (s *PendingApprovalStore) FindByToolCall(ctx context.Context, toolCallID string) (*PendingApproval, *PendingToolCall, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range all {
		for _, tc := range all[i].ParsedToolCalls() {
			if tc.ToolCallID == toolCallID {
				match := tc
				return &all[i], &match, nil
			}
		}
	}
	return nil, nil, ErrNotFound
}

// Delete removes the pending approval for runID, ErrNotFound when no row was
// deleted: deleting doubles as the exclusive claim on the approval.
func (s *PendingApprovalStore) Delete(ctx context.Context, runID string) error {
	res, err := s.db.NewDelete().Model((*PendingApproval)(nil)).
		Where("run_id = ?", runID).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("deleting pending approval %s: %w", runID, err)
	}
	return nil
}

// ListOlderThan returns the approvals filed before cutoff — the reaper's
// candidates. This read claims nothing; each row is claimed on its own
// (TaskStore.ClaimApprovalCancelled), so the reaper never acts on one it did not remove.
func (s *PendingApprovalStore) ListOlderThan(ctx context.Context, cutoff time.Time) ([]PendingApproval, error) {
	var out []PendingApproval
	if err := s.db.NewSelect().Model(&out).Where("created_at < ?", cutoff).OrderExpr("created_at ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing expired approvals: %w", err)
	}
	return out, nil
}
