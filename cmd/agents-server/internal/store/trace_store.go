package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// TraceStore persists and queries trace events.
type TraceStore struct {
	db *bun.DB
}

// NewTraceStore returns a TraceStore backed by db.
func NewTraceStore(db *bun.DB) *TraceStore {
	return &TraceStore{db: db}
}

// Insert stores a single trace event, stamping its created_at.
func (s *TraceStore) Insert(ctx context.Context, ev *TraceEvent) error {
	ev.CreatedAt = time.Now().UTC()
	if _, err := s.db.NewInsert().Model(ev).Exec(ctx); err != nil {
		return fmt.Errorf("inserting trace event: %w", err)
	}
	return nil
}

// InsertBatch stores multiple trace events in one insert, stamping their
// created_at; it is a no-op when events is empty.
func (s *TraceStore) InsertBatch(ctx context.Context, events []TraceEvent) error {
	if len(events) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for i := range events {
		events[i].CreatedAt = now
	}
	if _, err := s.db.NewInsert().Model(&events).Exec(ctx); err != nil {
		return fmt.Errorf("batch inserting trace events: %w", err)
	}
	return nil
}

// ListBySession returns trace events for sessionID ordered oldest first.
// limit > 0 selects the newest `limit` rows (optionally only those with
// id < beforeID); limit <= 0 returns everything.
func (s *TraceStore) ListBySession(ctx context.Context, sessionID string, beforeID int64, limit int) ([]TraceEvent, error) {
	var events []TraceEvent
	q := s.db.NewSelect().Model(&events).
		Where("session_id = ?", sessionID)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	if limit > 0 {
		q = q.OrderExpr("id DESC").Limit(limit)
	} else {
		q = q.OrderExpr("id ASC")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing trace events for session %s: %w", sessionID, err)
	}
	if limit > 0 {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}
	return events, nil
}

// DeleteOlderThan removes trace events created before cutoff. Returns the
// number of rows removed.
func (s *TraceStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.NewDelete().Model((*TraceEvent)(nil)).
		Where("created_at < ?", cutoff).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting trace events before %s: %w", cutoff, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ForkBySession copies trace events from srcSessionID to dstSessionID,
// limited to the given run IDs.
func (s *TraceStore) ForkBySession(ctx context.Context, srcSessionID, dstSessionID string, runIDs []string) error {
	if len(runIDs) == 0 {
		return nil
	}
	var events []TraceEvent
	if err := s.db.NewSelect().Model(&events).
		Where("session_id = ?", srcSessionID).
		Where("run_id IN (?)", bun.List(runIDs)).
		OrderExpr("id ASC").
		Scan(ctx); err != nil {
		return fmt.Errorf("fork traces read: %w", err)
	}
	if len(events) == 0 {
		return nil
	}
	for i := range events {
		events[i].ID = 0
		events[i].SessionID = dstSessionID
	}
	if _, err := s.db.NewInsert().Model(&events).Exec(ctx); err != nil {
		return fmt.Errorf("fork traces write: %w", err)
	}
	return nil
}

// DeleteBySession removes all trace events for sessionID.
func (s *TraceStore) DeleteBySession(ctx context.Context, sessionID string) error {
	if _, err := s.db.NewDelete().Model((*TraceEvent)(nil)).
		Where("session_id = ?", sessionID).
		Exec(ctx); err != nil {
		return fmt.Errorf("deleting trace events for session %s: %w", sessionID, err)
	}
	return nil
}
