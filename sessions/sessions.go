// Package sessions provides SQL-backed session.Storage implementations (SQLite
// and PostgreSQL) on uptrace/bun, as a separate module so the drivers never
// reach the core SDK's dependency graph. Entries are stored one row per entry,
// the whole entry serialized as JSON in one column: entry kinds and payloads
// are an open set, so a column per field would make every new kind a schema
// migration, and a build meeting an unknown kind must still read the row back.
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
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"

	"github.com/zzir/agents-go/agents/session"
)

// entry is the row model: EntryID, ParentID and Kind are lifted out of the
// JSON for indexed lookups; everything else stays in the blob (open set).
type entry struct {
	bun.BaseModel `bun:"table:agent_entries,alias:e"`

	ID        int64  `bun:"id,pk,autoincrement"`
	SessionID string `bun:"session_id,notnull"`
	// Gen is the session generation these entries belong to; see
	// session.Ref. Empty is the direct scope, which is a scope like any
	// other and not a wildcard.
	Gen string `bun:"gen,notnull"`
	// Seq is the entry's cursor position, allocated by session.PrepareAppend —
	// session-local, unlike the table-wide autoincrement id, so it survives a
	// fork or an export between stores.
	Seq      int64  `bun:"seq,notnull"`
	EntryID  string `bun:"entry_id,notnull"`
	ParentID string `bun:"parent_id"`
	Kind     string `bun:"kind,notnull"`
	Entry    string `bun:"entry,notnull"` // JSON of session.Entry
}

// sessionRow records a session's existence and metadata, so a repo can list
// sessions with no entries yet and "hidden" is a session property.
type sessionRow struct {
	bun.BaseModel `bun:"table:agent_sessions,alias:s"`

	ID string `bun:"id,pk"`
	// Gen names which generation of this id owns the session's entries.
	Gen       string    `bun:"gen,notnull"`
	Title     string    `bun:"title"`
	Hidden    bool      `bun:"hidden"`
	CreatedAt time.Time `bun:"created_at,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}

// Session is a bun-backed session.Storage scoped to one session ID. Multiple
// Sessions may share a *bun.DB with different IDs.
type Session struct {
	db  *bun.DB
	ref session.Ref
}

// New wraps an existing *bun.DB as a Session for the given session ID. The
// caller owns the db's lifecycle (and dialect). Use NewSQLite or NewPostgres for
// the common cases. Call CreateSchema once before first use.
func New(db *bun.DB, sessionID string) *Session {
	return &Session{db: db, ref: session.Direct(sessionID)}
}

// forRef is New for a repo-created session, addressed by one generation of an
// id. Every query goes through scoped, so no path can reach another one's rows.
func forRef(db *bun.DB, ref session.Ref) *Session {
	return &Session{db: db, ref: ref}
}

// scoped narrows a query to this session; reads and writes both go through it,
// making the generation part of the address rather than a forgettable field.
func (s *Session) scoped(q *bun.SelectQuery) *bun.SelectQuery {
	return q.Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen)
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
	tuneSQLite(sqldb)
	db := bun.NewDB(sqldb, sqlitedialect.New())
	return New(db, sessionID), db, nil
}

// tuneSQLite caps the pool at one connection: a second writer fails at once
// with SQLITE_BUSY, which a CAS UPDATE cannot tell from "lost"; pragmas are best-effort.
func tuneSQLite(sqldb *sql.DB) {
	sqldb.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		_, _ = sqldb.Exec(pragma)
	}
}

// NewPostgres wraps an existing PostgreSQL *sql.DB as a Session, returning the
// *bun.DB for further use. The caller owns the underlying connection pool.
func NewPostgres(sqldb *sql.DB, sessionID string) (*Session, *bun.DB) {
	db := bun.NewDB(sqldb, pgdialect.New())
	return New(db, sessionID), db
}

// CreateSchema creates the agent_entries table and its lookup indexes if they
// do not already exist. It is safe to call repeatedly. The entry indexes are
// UNIQUE, and that is load-bearing: sequence numbers and entry ids are never
// handed out twice (spec §2.5e2), so a duplicate becomes a failed write. This
// project ships no migrations — rebuild the database on a schema change.
func CreateSchema(ctx context.Context, db *bun.DB) error {
	// The task table comes with it (and CreateTaskSchema creates the session
	// table), so either entry point leaves a consistent schema.
	if err := CreateTaskSchema(ctx, db); err != nil {
		return err
	}
	if _, err := db.NewCreateTable().Model((*entry)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	if _, err := db.NewCreateIndex().
		Model((*entry)(nil)).
		Index("idx_agent_entries_session").
		Unique().
		Column("session_id", "gen", "seq").
		IfNotExists().
		Exec(ctx); err != nil {
		return err
	}
	// Point lookups by entry id: without this, resolving one entry means
	// reading the whole session.
	if _, err := db.NewCreateIndex().
		Model((*entry)(nil)).
		Index("idx_agent_entries_entry_id").
		Unique().
		Column("session_id", "gen", "entry_id").
		IfNotExists().
		Exec(ctx); err != nil {
		return err
	}
	// Repo.List: newest first among the visible sessions, without a scan.
	_, err := db.NewCreateIndex().
		Model((*sessionRow)(nil)).
		Index("idx_agent_sessions_listing").
		Column("hidden").
		ColumnExpr("updated_at DESC").
		IfNotExists().
		Exec(ctx)
	return err
}

// Entries implements session.Storage, paginating by cursor.
func (s *Session) Entries(ctx context.Context, cur session.Cursor) ([]session.Entry, error) {
	return s.entriesIn(ctx, s.db, cur)
}

func (s *Session) entriesIn(ctx context.Context, db bun.IDB, cur session.Cursor) ([]session.Entry, error) {
	var rows []entry
	q := s.scoped(db.NewSelect().Model(&rows))
	if cur.AfterSeq > 0 {
		q = q.Where("seq > ?", cur.AfterSeq)
	}
	// A negative limit means "the most recent N": read in reverse and flip below,
	// far cheaper than reading the whole session to slice its tail.
	limit := cur.Limit
	if limit < 0 {
		q = q.Order("seq DESC").Limit(-limit)
	} else {
		q = q.Order("seq ASC")
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
	out := make([]session.Entry, 0, len(rows))
	for _, r := range rows {
		e, err := decodeEntry(r)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// lockForWrite makes read-plan-write one step (spec §2.5e2): PostgreSQL takes
// a transaction advisory lock on (id, gen); SQLite relies on the one-connection pool.
func (s *Session) lockForWrite(ctx context.Context, tx bun.Tx) error {
	if s.db.Dialect().Name() != dialect.PG {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock(hashtext(?), hashtext(?))", s.ref.ID, s.ref.Gen)
	if err != nil {
		return fmt.Errorf("locking session for write: %w", err)
	}
	return nil
}

// touchIn records that the session changed and, for a repo-created session,
// proves it still EXISTS: zero rows means deleted, and the write rolls back (spec §2.5e2).
func (s *Session) touchIn(ctx context.Context, tx bun.Tx) error {
	res, err := tx.NewUpdate().Model((*sessionRow)(nil)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
		Exec(ctx)
	if err != nil {
		return err
	}
	if s.ref.Gen == "" {
		return nil
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return fmt.Errorf("session %s: %w", s.ref.ID, session.ErrNotFound)
	}
	return nil
}

// Append implements session.Storage. The append point is read inside the
// same transaction as the insert — see lockForWrite for why.
func (s *Session) Append(ctx context.Context, entries ...session.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.lockForWrite(ctx, tx); err != nil {
			return err
		}
		at, err := s.appendPointIn(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := s.encodeEntries(entries, at)
		if err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return err
		}
		return s.touchIn(ctx, tx)
	})
}

// Clear implements session.Storage, removing every entry for this session ID
// under the same write lock every other entry write holds — an unlocked clear
// could land between an append's tip-read and its insert.
func (s *Session) Clear(ctx context.Context) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := s.lockForWrite(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*entry)(nil)).
			Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).Exec(ctx); err != nil {
			return err
		}
		return s.touchIn(ctx, tx)
	})
}

// ReplaceEntries implements session.AtomicReplacer: the delete of the old
// history and the insert of the new one run in a single transaction, so a
// failure mid-rewrite rolls back to the previous history instead of leaving the
// session empty. Only this session ID's rows are touched. The high-water mark
// is read inside the same transaction — see lockForWrite.
func (s *Session) ReplaceEntries(ctx context.Context, entries ...session.Entry) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := s.replaceIn(ctx, tx, entries, nil)
		return err
	})
}

// ReplaceEntriesIf implements session.GuardedReplacer. The high-water mark the
// guard compares is read by the transaction that carries the rewrite, holding
// the same write lock every append holds — so an append cannot commit between
// the comparison and the swap.
func (s *Session) ReplaceEntriesIf(ctx context.Context, expect int64, entries ...session.Entry) (bool, error) {
	replaced := false
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		replaced, err = s.replaceIn(ctx, tx, entries, &expect)
		return err
	})
	if err != nil {
		return false, err
	}
	return replaced, nil
}

// replaceIn swaps this session's rows for entries on the caller's transaction
// and reports whether it wrote; a non-nil expect guards on the highest seq.
func (s *Session) replaceIn(ctx context.Context, tx bun.Tx, entries []session.Entry, expect *int64) (bool, error) {
	if err := s.lockForWrite(ctx, tx); err != nil {
		return false, err
	}
	// A replace does not restart the numbering — a cursor outlives the entries it
	// pointed at — so the high-water mark carries over while the branch starts fresh.
	at, err := s.appendPointIn(ctx, tx)
	if err != nil {
		return false, err
	}
	if expect != nil && at.LastSeq != *expect {
		return false, nil
	}
	rows, err := s.encodeEntries(entries, session.AppendPoint{LastSeq: at.LastSeq})
	if err != nil {
		return false, err
	}
	if _, err := tx.NewDelete().Model((*entry)(nil)).
		Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).Exec(ctx); err != nil {
		return false, err
	}
	if len(rows) > 0 {
		if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return false, err
		}
	}
	if err := s.touchIn(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}

// encodeEntries prepares entries for insertion, filling in the store-owned
// fields; a caller-supplied id is kept so a fork or replace preserves identity.
func (s *Session) encodeEntries(entries []session.Entry, at session.AppendPoint) ([]entry, error) {
	prepared := session.PrepareAppend(entries, at)
	rows := make([]entry, 0, len(prepared))
	for _, e := range prepared {
		data, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("encoding session entry: %w", err)
		}
		rows = append(rows, entry{
			SessionID: s.ref.ID,
			Gen:       s.ref.Gen,
			Seq:       e.Seq,
			EntryID:   e.ID,
			ParentID:  e.ParentID,
			Kind:      string(e.Kind),
			Entry:     string(data),
		})
	}
	return rows, nil
}

// appendPointIn reads the branch tip on the caller's transaction, so tip and
// write are one step: the newest row, or for a leaf move the entry it points at.
func (s *Session) appendPointIn(ctx context.Context, db bun.IDB) (session.AppendPoint, error) {
	var row entry
	err := s.scoped(db.NewSelect().Model(&row)).
		Order("seq DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return session.AppendPoint{}, nil
	}
	if err != nil {
		return session.AppendPoint{}, err
	}
	e, derr := decodeEntry(row)
	if derr != nil {
		return session.AppendPoint{}, derr
	}
	return session.AppendPoint{
		Leaf:    session.LeafOf([]session.Entry{e}),
		LastSeq: row.Seq,
	}, nil
}

func decodeEntry(r entry) (session.Entry, error) {
	var e session.Entry
	if err := json.Unmarshal([]byte(r.Entry), &e); err != nil {
		return session.Entry{}, fmt.Errorf("decoding session entry %d: %w", r.ID, err)
	}
	return e, nil
}

var (
	_ session.Storage         = (*Session)(nil)
	_ session.AtomicReplacer  = (*Session)(nil)
	_ session.GuardedReplacer = (*Session)(nil)
)

// Metadata implements session.Storage. It counts rather than loading, and
// merges in the session row when one exists (a session created through a repo).
func (s *Session) Metadata(ctx context.Context) (session.Metadata, error) {
	n, err := s.scoped(s.db.NewSelect().Model((*entry)(nil))).Count(ctx)
	if err != nil {
		return session.Metadata{}, err
	}
	md := session.Metadata{ID: s.ref.ID, EntryCount: n}

	var row sessionRow
	err = s.db.NewSelect().Model(&row).
		Where("id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).Limit(1).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A session used directly, without a repo: entries are all there is.
		return md, nil
	case err != nil:
		return md, err
	}
	md.Title, md.Hidden = row.Title, row.Hidden
	md.CreatedAt, md.UpdatedAt = row.CreatedAt, row.UpdatedAt
	return md, nil
}

// Entry implements session.Storage with an indexed lookup rather than a
// scan of the session.
func (s *Session) Entry(ctx context.Context, id string) (*session.Entry, error) {
	var row entry
	err := s.scoped(s.db.NewSelect().Model(&row)).
		Where("entry_id = ?", id).
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e, derr := decodeEntry(row)
	if derr != nil {
		return nil, derr
	}
	return &e, nil
}
