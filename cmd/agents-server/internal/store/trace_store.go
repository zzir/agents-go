package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
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

// ListBySession returns trace events for sessionID ordered oldest first.
// limit > 0 selects the newest `limit` rows (optionally only those with
// id < beforeID); limit <= 0 returns everything.
func (s *TraceStore) ListBySession(ctx context.Context, sessionID string, beforeID string, limit int) ([]TraceEvent, error) {
	return s.list(ctx, sessionID, beforeID, limit, false)
}

// payloadFields are a span's payload proper — the model's request and reply,
// a tool's arguments and result — as opposed to what a listing shows on the
// row (name, timings, tokens, error). They are nearly all of a session's trace
// bytes: a generation span carries the whole conversation as its input.
var payloadFields = []string{"input", "output", "system_instructions", "tools", "handoffs", "output_schema"}

// ListSummaryBySession is ListBySession with the payload fields left out of
// each row's Data (PayloadOmitted marks the rows that had any), done in the
// database so nothing bulky is read, sent or parsed until GetBySpan is asked
// for one span.
func (s *TraceStore) ListSummaryBySession(ctx context.Context, sessionID string, beforeID string, limit int) ([]TraceEvent, error) {
	return s.list(ctx, sessionID, beforeID, limit, true)
}

func (s *TraceStore) list(ctx context.Context, sessionID string, beforeID string, limit int, summary bool) ([]TraceEvent, error) {
	var events []TraceEvent
	q := s.db.NewSelect().Model(&events).
		Where("session_id = ?", sessionID)
	if summary {
		// A row whose data is not JSON (there is none such from this build)
		// is passed through as it is rather than failing the whole listing.
		var dataExpr, omittedExpr string
		if s.db.Dialect().Name() == dialect.PG {
			removed := "te.data::jsonb"
			exists := make([]string, len(payloadFields))
			for i, f := range payloadFields {
				removed += " - '" + f + "'"
				exists[i] = "jsonb_exists(te.data::jsonb, '" + f + "')"
			}
			dataExpr = "CASE WHEN te.data IS JSON THEN (" + removed + ")::text ELSE te.data END AS data"
			omittedExpr = "CASE WHEN te.data IS JSON THEN (" + strings.Join(exists, " OR ") + ") ELSE false END AS payload_omitted"
		} else {
			paths := make([]string, len(payloadFields))
			exists := make([]string, len(payloadFields))
			for i, f := range payloadFields {
				paths[i] = "'$." + f + "'"
				exists[i] = "json_type(te.data, '$." + f + "') IS NOT NULL"
			}
			dataExpr = "CASE WHEN json_valid(te.data) THEN json_remove(te.data, " + strings.Join(paths, ", ") + ") ELSE te.data END AS data"
			omittedExpr = "CASE WHEN json_valid(te.data) THEN (" + strings.Join(exists, " OR ") + ") ELSE 0 END AS payload_omitted"
		}
		q = q.ExcludeColumn("data").ColumnExpr(dataExpr).ColumnExpr(omittedExpr)
	}
	if beforeID != "" {
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

// GetBySpan returns one span's row, payload included, or an ErrNotFound-wrapping
// error — what a summary listing's PayloadOmitted row is opened with.
func (s *TraceStore) GetBySpan(ctx context.Context, sessionID, spanID string) (*TraceEvent, error) {
	ev := new(TraceEvent)
	err := s.db.NewSelect().Model(ev).
		Where("session_id = ?", sessionID).Where("span_id = ?", spanID).
		OrderExpr("id DESC").Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting trace span %s of session %s: %w", spanID, sessionID, err)
	}
	return ev, nil
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
		events[i].ID = "" // minted afresh on insert
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
