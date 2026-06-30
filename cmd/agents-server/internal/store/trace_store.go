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

// ListBySession returns all trace events for sessionID ordered oldest first.
func (s *TraceStore) ListBySession(ctx context.Context, sessionID string) ([]TraceEvent, error) {
	var events []TraceEvent
	if err := s.db.NewSelect().Model(&events).
		Where("session_id = ?", sessionID).
		OrderExpr("id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing trace events for session %s: %w", sessionID, err)
	}
	return events, nil
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
