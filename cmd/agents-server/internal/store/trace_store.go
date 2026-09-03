package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/uptrace/bun"
)

// blobBatch bounds one statement's element count: an IN list or a multi-row
// insert of hashes, under every driver's bind limit.
const blobBatch = 500

// TraceStore persists and queries trace events. A span's payload is held in
// trace_blobs, referenced by hash from the span row (decisions §5.50).
type TraceStore struct {
	db *bun.DB
}

// NewTraceStore returns a TraceStore backed by db.
func NewTraceStore(db *bun.DB) *TraceStore {
	return &TraceStore{db: db}
}

// SpanWriter inserts one run's spans, remembering which elements the session
// already holds so a generation span writes only the items new since the last call.
type SpanWriter struct {
	s         *TraceStore
	sessionID string
	elemCap   int
	mu        sync.Mutex
	seen      map[[hashSize]byte]struct{} // nil until the first insert warms it
}

// NewSpanWriter returns a writer for sessionID's spans; elemCap bounds one
// payload element in bytes (0: unbounded).
func (s *TraceStore) NewSpanWriter(sessionID string, elemCap int) *SpanWriter {
	return &SpanWriter{s: s, sessionID: sessionID, elemCap: elemCap}
}

// Insert stores ev as a span of the writer's session.
func (w *SpanWriter) Insert(ctx context.Context, ev *TraceEvent) error {
	ev.SessionID = w.sessionID
	w.mu.Lock()
	if w.seen == nil {
		if err := w.warm(ctx); err != nil {
			w.mu.Unlock()
			return err
		}
	}
	w.mu.Unlock()
	return w.s.insert(ctx, ev, w.elemCap, w)
}

// warm loads the hashes the session already holds. Called under mu.
func (w *SpanWriter) warm(ctx context.Context) error {
	var hashes [][]byte
	if err := w.s.db.NewSelect().Model((*TraceBlob)(nil)).Column("hash").
		Where("session_id = ?", w.sessionID).Scan(ctx, &hashes); err != nil {
		return fmt.Errorf("loading trace blob hashes of session %s: %w", w.sessionID, err)
	}
	w.seen = make(map[[hashSize]byte]struct{}, len(hashes))
	for _, h := range hashes {
		if len(h) == hashSize {
			w.seen[[hashSize]byte(h)] = struct{}{}
		}
	}
	return nil
}

func (w *SpanWriter) has(h [hashSize]byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.seen[h]
	return ok
}

func (w *SpanWriter) add(hashes [][hashSize]byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range hashes {
		w.seen[h] = struct{}{}
	}
}

// Insert stores a single trace event, its payload split into trace_blobs.
// Uncached: a run's spans go through a SpanWriter.
func (s *TraceStore) Insert(ctx context.Context, ev *TraceEvent) error {
	return s.insert(ctx, ev, 0, nil)
}

// insert splits ev's payload, writes the unseen elements and the row in one
// transaction, and only then tells the writer what landed.
func (s *TraceStore) insert(ctx context.Context, ev *TraceEvent, elemCap int, w *SpanWriter) error {
	ev.CreatedAt = time.Now().UTC()
	meta, layout, elems := splitPayload(ev.Data, elemCap)
	ev.Data, ev.Layout, ev.Refs = meta, "", nil
	var blobs []TraceBlob
	var written [][hashSize]byte
	if layout != nil {
		lb, err := json.Marshal(layout)
		if err != nil {
			return fmt.Errorf("encoding trace layout: %w", err)
		}
		ev.Layout = string(lb)
		hashes := make([][hashSize]byte, len(elems))
		inSpan := map[[hashSize]byte]struct{}{}
		for i, e := range elems {
			h := sha256.Sum256(e)
			hashes[i] = h
			if _, dup := inSpan[h]; dup || (w != nil && w.has(h)) {
				continue
			}
			inSpan[h] = struct{}{}
			written = append(written, h)
			blobs = append(blobs, TraceBlob{SessionID: ev.SessionID, Hash: h[:], Body: encodeBody(e)})
		}
		ev.Refs = packRefs(hashes)
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for chunk := range slices.Chunk(blobs, blobBatch) {
			if _, err := tx.NewInsert().Model(&chunk).On("CONFLICT (session_id, hash) DO NOTHING").Exec(ctx); err != nil {
				return err
			}
		}
		_, err := tx.NewInsert().Model(ev).Exec(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("inserting trace event: %w", err)
	}
	if w != nil {
		w.add(written)
	}
	return nil
}

// ListBySession returns trace events for sessionID oldest first, payload
// inlined. limit > 0 selects the newest `limit` rows (id < beforeID when set).
func (s *TraceStore) ListBySession(ctx context.Context, sessionID string, beforeID string, limit int) ([]TraceEvent, error) {
	return s.list(ctx, sessionID, beforeID, limit, false)
}

// ListSummaryBySession is ListBySession without the payload: rows that have
// one are marked PayloadOmitted, and GetBySpan serves it.
func (s *TraceStore) ListSummaryBySession(ctx context.Context, sessionID string, beforeID string, limit int) ([]TraceEvent, error) {
	return s.list(ctx, sessionID, beforeID, limit, true)
}

func (s *TraceStore) list(ctx context.Context, sessionID string, beforeID string, limit int, summary bool) ([]TraceEvent, error) {
	var events []TraceEvent
	q := s.db.NewSelect().Model(&events).
		Where("session_id = ?", sessionID)
	if summary {
		q = q.ExcludeColumn("layout", "refs").ColumnExpr("te.layout IS NOT NULL AS payload_omitted")
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
		slices.Reverse(events)
	}
	if !summary {
		if err := s.inlineAll(ctx, sessionID, events); err != nil {
			return nil, fmt.Errorf("listing trace events for session %s: %w", sessionID, err)
		}
	}
	return events, nil
}

// inlineAll rebuilds every row's payload from one read of the session's
// blobs — bounded by the session's deduplicated content, not by its spans.
func (s *TraceStore) inlineAll(ctx context.Context, sessionID string, events []TraceEvent) error {
	need := slices.ContainsFunc(events, func(ev TraceEvent) bool { return ev.Layout != "" })
	if !need {
		return nil
	}
	var blobs []TraceBlob
	if err := s.db.NewSelect().Model(&blobs).Column("hash", "body").
		Where("session_id = ?", sessionID).Scan(ctx); err != nil {
		return err
	}
	bodies := make(map[[hashSize]byte][]byte, len(blobs))
	for _, b := range blobs {
		if len(b.Hash) == hashSize {
			bodies[[hashSize]byte(b.Hash)] = b.Body
		}
	}
	for i := range events {
		if err := inlinePayload(&events[i], bodies); err != nil {
			return err
		}
	}
	return nil
}

// inlinePayload puts ev's payload back into Data from bodies; an element
// whose blob is missing reads as pruned.
func inlinePayload(ev *TraceEvent, bodies map[[hashSize]byte][]byte) error {
	if ev.Layout == "" {
		return nil
	}
	var layout []layoutField
	if err := json.Unmarshal([]byte(ev.Layout), &layout); err != nil {
		return fmt.Errorf("decoding trace layout of span %s: %w", ev.SpanID, err)
	}
	elems := make([][]byte, layoutTotal(layout))
	if hashes, ok := unpackRefs(ev.Refs); ok && len(hashes) == len(elems) {
		for i, h := range hashes {
			if body, ok := bodies[h]; ok {
				if e, err := decodeBody(body); err == nil {
					elems[i] = e
				}
			}
		}
	}
	data, err := joinPayload(ev.Data, layout, elems)
	if err != nil {
		return fmt.Errorf("rebuilding trace payload of span %s: %w", ev.SpanID, err)
	}
	ev.Data = data
	return nil
}

// GetBySpan returns one span's row, payload inlined, or an
// ErrNotFound-wrapping error.
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
	if ev.Layout == "" {
		return ev, nil
	}
	hashes, _ := unpackRefs(ev.Refs)
	bodies := make(map[[hashSize]byte][]byte, len(hashes))
	for chunk := range slices.Chunk(hashes, blobBatch) {
		keys := make([][]byte, len(chunk))
		for i := range chunk {
			keys[i] = chunk[i][:]
		}
		var blobs []TraceBlob
		if err := s.db.NewSelect().Model(&blobs).Column("hash", "body").
			Where("session_id = ?", sessionID).Where("hash IN (?)", bun.List(keys)).Scan(ctx); err != nil {
			return nil, fmt.Errorf("getting trace span %s of session %s: %w", spanID, sessionID, err)
		}
		for _, b := range blobs {
			if len(b.Hash) == hashSize {
				bodies[[hashSize]byte(b.Hash)] = b.Body
			}
		}
	}
	if err := inlinePayload(ev, bodies); err != nil {
		return nil, err
	}
	return ev, nil
}

// DeleteOlderThan removes trace events created before cutoff, then the blobs
// of every session left with none. Returns the number of event rows removed.
func (s *TraceStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := deleteInBatches(ctx, s.db, (*TraceEvent)(nil), func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("created_at < ?", cutoff)
	})
	if err != nil {
		return n, fmt.Errorf("deleting trace events before %s: %w", cutoff, err)
	}
	if n == 0 {
		return 0, nil
	}
	var orphaned []string
	if err := s.db.NewSelect().TableExpr("trace_blobs AS tb").ColumnExpr("DISTINCT tb.session_id").
		Where("NOT EXISTS (SELECT 1 FROM trace_events te WHERE te.session_id = tb.session_id)").
		Scan(ctx, &orphaned); err != nil {
		return n, fmt.Errorf("finding trace blobs without events: %w", err)
	}
	for _, sid := range orphaned {
		if _, err := s.db.NewDelete().Model((*TraceBlob)(nil)).Where("session_id = ?", sid).Exec(ctx); err != nil {
			return n, fmt.Errorf("deleting trace blobs of session %s: %w", sid, err)
		}
	}
	return n, nil
}

// PrunePayloadBefore drops the blobs of every session whose newest trace
// event is older than cutoff; the rows stay. Returns the number of sessions pruned.
func (s *TraceStore) PrunePayloadBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var sessions []string
	if err := s.db.NewSelect().Model((*TraceEvent)(nil)).Column("session_id").
		GroupExpr("session_id").
		Having("MAX(created_at) < ?", cutoff).
		Having("COUNT(layout) > 0").
		Scan(ctx, &sessions); err != nil {
		return 0, fmt.Errorf("finding sessions with prunable trace payload: %w", err)
	}
	for _, sid := range sessions {
		err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewDelete().Model((*TraceBlob)(nil)).Where("session_id = ?", sid).Exec(ctx); err != nil {
				return err
			}
			_, err := tx.NewUpdate().Model((*TraceEvent)(nil)).
				Set("layout = NULL").Set("refs = NULL").
				Where("session_id = ?", sid).Exec(ctx)
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("pruning trace payload of session %s: %w", sid, err)
		}
	}
	return len(sessions), nil
}

// ForkBySession copies the trace events of the given runs from srcSessionID
// to dstSessionID, with exactly the blobs they reference.
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
	referenced := map[[hashSize]byte]struct{}{}
	for i := range events {
		events[i].ID = "" // minted afresh on insert
		events[i].SessionID = dstSessionID
		hashes, _ := unpackRefs(events[i].Refs)
		for _, h := range hashes {
			referenced[h] = struct{}{}
		}
	}
	keys := make([][]byte, 0, len(referenced))
	for h := range referenced {
		keys = append(keys, h[:])
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(&events).Exec(ctx); err != nil {
			return err
		}
		for chunk := range slices.Chunk(keys, blobBatch) {
			if _, err := tx.NewRaw(
				"INSERT INTO trace_blobs (session_id, hash, body) SELECT ?, hash, body FROM trace_blobs WHERE session_id = ? AND hash IN (?) ON CONFLICT DO NOTHING",
				dstSessionID, srcSessionID, bun.List(chunk)).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fork traces write: %w", err)
	}
	return nil
}

// DeleteBySession removes all trace events and blobs for sessionID.
func (s *TraceStore) DeleteBySession(ctx context.Context, sessionID string) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, model := range []any{(*TraceEvent)(nil), (*TraceBlob)(nil)} {
			if _, err := tx.NewDelete().Model(model).Where("session_id = ?", sessionID).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("deleting trace events for session %s: %w", sessionID, err)
	}
	return nil
}
