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

// List returns every pending approval, oldest first (FindByToolCall scans it;
// recovery after a restart is lazy — approvals resume from these rows on the
// next approve/reject, nothing is preloaded).
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
// given tool call id. Matching is done in Go so it doesn't depend on the JSON
// storage shape.
func (s *PendingApprovalStore) FindByToolCall(ctx context.Context, toolCallID string) (*PendingApproval, *PendingToolCall, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range all {
		for _, tc := range all[i].parsedToolCalls() {
			if tc.ToolCallID == toolCallID {
				match := tc
				return &all[i], &match, nil
			}
		}
	}
	return nil, nil, ErrNotFound
}

// Delete removes the pending approval for runID. It returns an
// ErrNotFound-wrapping error when no row was deleted — deleting doubles as
// the exclusive claim on the approval, so concurrent decisions race here and
// exactly one wins.
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

// DeleteOlderThan removes pending approvals created before cutoff and returns
// the removed rows (so the caller can annotate the affected sessions).
func (s *PendingApprovalStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) ([]PendingApproval, error) {
	var removed []PendingApproval
	if err := s.db.NewSelect().Model(&removed).
		Where("created_at < ?", cutoff).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("scanning expired approvals: %w", err)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if _, err := s.db.NewDelete().Model((*PendingApproval)(nil)).
		Where("created_at < ?", cutoff).
		Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting expired approvals: %w", err)
	}
	return removed, nil
}
