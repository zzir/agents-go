// Package sessions provides SQL-backed agents.Session implementations (SQLite
// and PostgreSQL) built on uptrace/bun. It is a separate Go module so the
// database driver dependencies never reach the core SDK's dependency graph —
// callers who use only InMemorySession or memory.FileSession pay nothing for it.
//
// Session entries are stored one row per entry, the whole entry serialized as
// JSON in a single column. Entry kinds and their payloads are an open set, so a
// column per field would make every new kind a schema migration — and a build
// that meets a kind it does not know must still read the row back intact.
package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"

	"github.com/zzir/agents-go/agents"
)

// entry is the row model: one stored session entry, ordered by the
// autoincrement id within a session.
//
// The whole entry is stored as JSON in one column rather than spread across
// typed columns. Kinds and their payloads are an open set — a build that meets
// an entry kind it does not know must still read the row back intact — and a
// column per field would make every new kind a schema migration.
type entry struct {
	bun.BaseModel `bun:"table:agent_entries,alias:e"`

	ID        int64  `bun:"id,pk,autoincrement"`
	SessionID string `bun:"session_id,notnull"`
	Kind      string `bun:"kind,notnull"`  // indexed for kind-filtered reads
	Entry     string `bun:"entry,notnull"` // JSON of agents.SessionEntry
}

// Session is a bun-backed agents.Session scoped to one session ID. Multiple
// Sessions may share a *bun.DB with different IDs.
type Session struct {
	db        *bun.DB
	sessionID string
}

// New wraps an existing *bun.DB as a Session for the given session ID. The
// caller owns the db's lifecycle (and dialect). Use NewSQLite or NewPostgres for
// the common cases. Call CreateSchema once before first use.
func New(db *bun.DB, sessionID string) *Session {
	return &Session{db: db, sessionID: sessionID}
}

// NewSQLite opens a SQLite database at dsn (e.g. "file:agents.db?cache=shared",
// or "file::memory:?cache=shared") using the pure-Go modernc driver — no CGO —
// and returns a Session plus the *bun.DB so the caller can Close it. It does not
// create the schema; call CreateSchema.
func NewSQLite(dsn, sessionID string) (*Session, *bun.DB, error) {
	sqldb, err := sql.Open(sqliteshim.ShimName, dsn)
	if err != nil {
		return nil, nil, err
	}
	db := bun.NewDB(sqldb, sqlitedialect.New())
	return New(db, sessionID), db, nil
}

// NewPostgres wraps an existing PostgreSQL *sql.DB as a Session, returning the
// *bun.DB for further use. The caller owns the underlying connection pool.
func NewPostgres(sqldb *sql.DB, sessionID string) (*Session, *bun.DB) {
	db := bun.NewDB(sqldb, pgdialect.New())
	return New(db, sessionID), db
}

// CreateSchema creates the agent_entries table and its lookup index if they do
// not already exist. It is safe to call repeatedly.
//
// The schema changed shape when sessions moved from items to entries; there is
// no migration from agent_messages. This project does not ship migrations —
// rebuild the database.
func CreateSchema(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*entry)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	_, err := db.NewCreateIndex().
		Model((*entry)(nil)).
		Index("idx_agent_entries_session").
		Column("session_id", "id").
		IfNotExists().
		Exec(ctx)
	return err
}

// Entries implements agents.SessionStorage, paginating by cursor.
func (s *Session) Entries(ctx context.Context, cur agents.Cursor) ([]agents.SessionEntry, error) {
	var rows []entry
	q := s.db.NewSelect().Model(&rows).Where("session_id = ?", s.sessionID)
	if cur.AfterSeq > 0 {
		q = q.Where("id > ?", cur.AfterSeq)
	}
	// A negative limit means "the most recent N", which the database answers by
	// reading in reverse and flipping below — far cheaper than reading the
	// whole session to slice its tail.
	limit := cur.Limit
	if limit < 0 {
		q = q.Order("id DESC").Limit(-limit)
	} else {
		q = q.Order("id ASC")
		if limit > 0 {
			q = q.Limit(limit)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	if limit < 0 {
		slices.Reverse(rows) // most-recent-first -> oldest-first
	}
	out := make([]agents.SessionEntry, 0, len(rows))
	for _, r := range rows {
		e, err := decodeEntry(r)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Append implements agents.SessionStorage.
func (s *Session) Append(ctx context.Context, entries ...agents.SessionEntry) error {
	if len(entries) == 0 {
		return nil
	}
	rows, err := s.encodeEntries(entries)
	if err != nil {
		return err
	}
	_, err = s.db.NewInsert().Model(&rows).Exec(ctx)
	return err
}

// PopEntry implements agents.Session, removing and returning the most recent
// entry (or nil if the session is empty).
//
// Concurrency: a plain transaction with SELECT max(id) then DELETE does not
// stop two concurrent pops from reading the same row under PostgreSQL READ
// COMMITTED (both see the same max, both "delete" it, both return it).
// Instead each attempt selects a candidate row and then issues DELETE ...
// WHERE id = ?, which is atomic at the row level on both SQLite and
// PostgreSQL: exactly one deleter observes RowsAffected == 1. A loser
// (RowsAffected == 0) retries with the next candidate. Every retry means some
// other pop succeeded, so the loop is lock-free and terminates when the
// session runs empty.
func (s *Session) PopEntry(ctx context.Context) (*agents.SessionEntry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var row entry
		err := s.db.NewSelect().Model(&row).
			Where("session_id = ?", s.sessionID).
			Order("id DESC").Limit(1).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		res, err := s.db.NewDelete().Model((*entry)(nil)).Where("id = ?", row.ID).Exec(ctx)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			// A concurrent pop claimed this row between our select and
			// delete; try again with whatever is now the most recent entry.
			continue
		}
		e, err := decodeEntry(row)
		if err != nil {
			return nil, err
		}
		return &e, nil
	}
}

// Clear implements agents.Session, removing every entry for this session ID.
func (s *Session) Clear(ctx context.Context) error {
	_, err := s.db.NewDelete().Model((*entry)(nil)).Where("session_id = ?", s.sessionID).Exec(ctx)
	return err
}

// ReplaceEntries implements agents.EntriesReplacer: the delete of the old
// history and the insert of the new one run in a single transaction, so a
// failure mid-rewrite rolls back to the previous history instead of leaving the
// session empty. Only this session ID's rows are touched.
func (s *Session) ReplaceEntries(ctx context.Context, entries ...agents.SessionEntry) error {
	rows, err := s.encodeEntries(entries)
	if err != nil {
		return err
	}
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*entry)(nil)).Where("session_id = ?", s.sessionID).Exec(ctx); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		_, err := tx.NewInsert().Model(&rows).Exec(ctx)
		return err
	})
}

// encodeEntries prepares entries for insertion, filling in the fields the store
// owns. A caller-supplied id is kept, so an entry re-added by a fork or a
// replace keeps the identity an update entry points at.
func (s *Session) encodeEntries(entries []agents.SessionEntry) ([]entry, error) {
	rows := make([]entry, 0, len(entries))
	for i, e := range entries {
		if e.Kind == "" {
			e.Kind = agents.EntryKindItem
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Now().UTC()
		}
		if e.ID == "" {
			// The row's autoincrement id is not known before the insert, so the
			// entry id is minted here instead. It only has to be unique within
			// the session, which is all an update entry needs to point at one.
			e.ID = fmt.Sprintf("%s-%d-%d", s.sessionID, time.Now().UnixNano(), i)
		}
		data, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("encoding session entry: %w", err)
		}
		rows = append(rows, entry{SessionID: s.sessionID, Kind: string(e.Kind), Entry: string(data)})
	}
	return rows, nil
}

func decodeEntry(r entry) (agents.SessionEntry, error) {
	var e agents.SessionEntry
	if err := json.Unmarshal([]byte(r.Entry), &e); err != nil {
		return agents.SessionEntry{}, fmt.Errorf("decoding session entry %d: %w", r.ID, err)
	}
	// The row's autoincrement id IS the sequence number: it is what a cursor
	// pages on, and it is assigned by the database rather than guessed here.
	e.Seq = r.ID
	return e, nil
}

var (
	_ agents.SessionStorage = (*Session)(nil)
	_ agents.AtomicReplacer = (*Session)(nil)
	_ agents.EntryPopper    = (*Session)(nil)
)

// Metadata implements agents.SessionStorage.
func (s *Session) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	n, err := s.db.NewSelect().Model((*entry)(nil)).Where("session_id = ?", s.sessionID).Count(ctx)
	if err != nil {
		return agents.SessionMetadata{}, err
	}
	return agents.SessionMetadata{ID: s.sessionID, EntryCount: n}, nil
}

// Entry implements agents.SessionStorage.
func (s *Session) Entry(ctx context.Context, id string) (*agents.SessionEntry, error) {
	var rows []entry
	if err := s.db.NewSelect().Model(&rows).
		Where("session_id = ?", s.sessionID).Order("id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, r := range rows {
		e, err := decodeEntry(r)
		if err != nil {
			return nil, err
		}
		if e.ID == id {
			return &e, nil
		}
	}
	return nil, nil
}
