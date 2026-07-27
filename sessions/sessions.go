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
// EntryID, ParentID and Kind are lifted out of the JSON so the database can
// answer point lookups and kind filters with an index, instead of the whole
// session being loaded and scanned in Go. Everything else stays in the blob:
// entry kinds and payloads are an open set, and a column per field would make
// each new kind a schema migration.
type entry struct {
	bun.BaseModel `bun:"table:agent_entries,alias:e"`

	ID        int64  `bun:"id,pk,autoincrement"`
	SessionID string `bun:"session_id,notnull"`
	// Seq is the entry's cursor position, allocated by agents.PrepareAppend.
	//
	// It is a column rather than the row's autoincrement id because the two are
	// not the same number: an id is unique per TABLE and assigned on insert,
	// while Seq is the session-local position a Cursor pages on and has to
	// survive a fork or an export that carries entries between stores.
	Seq int64 `bun:"seq,notnull"`
	EntryID   string `bun:"entry_id,notnull"`
	ParentID  string `bun:"parent_id"`
	Kind      string `bun:"kind,notnull"`
	Entry     string `bun:"entry,notnull"` // JSON of agents.SessionEntry
}

// sessionRow records a session's existence and metadata, so a repo can list
// sessions that hold no entries yet and so "hidden" is a property of the
// session rather than something inferred from its contents.
type sessionRow struct {
	bun.BaseModel `bun:"table:agent_sessions,alias:s"`

	ID        string    `bun:"id,pk"`
	Title     string    `bun:"title"`
	Hidden    bool      `bun:"hidden"`
	CreatedAt time.Time `bun:"created_at,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
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
	tuneSQLite(sqldb)
	db := bun.NewDB(sqldb, sqlitedialect.New())
	return New(db, sessionID), db, nil
}

// tuneSQLite makes concurrent writers work at all.
//
// SQLite allows one writer, and with the default busy timeout of zero a second
// one fails IMMEDIATELY with SQLITE_BUSY rather than waiting. That turns every
// ordinary race — two runs finishing at once, a stop meeting a completion —
// into an error, and a conditional UPDATE used as a compare-and-set then cannot
// tell "somebody else won" from "the database was busy", which is the
// difference between correct and silently broken.
//
// Capping the pool at one connection is the portable fix. A busy_timeout pragma
// would be finer, but it applies PER CONNECTION and database/sql hands out
// connections from a pool, so a pragma executed once lands on whichever one it
// happened to get — and the DSN syntax for setting it differs between the
// drivers sqliteshim may resolve to. Serializing in Go costs a little
// throughput on a database that has one writer regardless.
//
// The pragmas that follow are best-effort: an in-memory or read-only database
// rejects them, which is not a reason to fail to open.
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

// CreateSchema creates the agent_entries table and its lookup index if they do
// not already exist. It is safe to call repeatedly.
//
// The schema changed shape when sessions moved from items to entries; there is
// no migration from agent_messages. This project does not ship migrations —
// rebuild the database.
func CreateSchema(ctx context.Context, db *bun.DB) error {
	for _, model := range []any{(*entry)(nil), (*sessionRow)(nil)} {
		if _, err := db.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return err
		}
	}
	if _, err := db.NewCreateIndex().
		Model((*entry)(nil)).
		Index("idx_agent_entries_session").
		Column("session_id", "seq").
		IfNotExists().
		Exec(ctx); err != nil {
		return err
	}
	// Point lookups by entry id: without this, resolving one entry means
	// reading the whole session.
	_, err := db.NewCreateIndex().
		Model((*entry)(nil)).
		Index("idx_agent_entries_entry_id").
		Column("session_id", "entry_id").
		IfNotExists().
		Exec(ctx)
	return err
}

// Entries implements agents.SessionStorage, paginating by cursor.
func (s *Session) Entries(ctx context.Context, cur agents.Cursor) ([]agents.SessionEntry, error) {
	var rows []entry
	q := s.db.NewSelect().Model(&rows).Where("session_id = ?", s.sessionID)
	if cur.AfterSeq > 0 {
		q = q.Where("seq > ?", cur.AfterSeq)
	}
	// A negative limit means "the most recent N", which the database answers by
	// reading in reverse and flipping below — far cheaper than reading the
	// whole session to slice its tail.
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
	at, err := s.appendPoint(ctx)
	if err != nil {
		return err
	}
	rows, err := s.encodeEntries(ctx, entries, at)
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
	// A replace does not restart the numbering — a cursor outlives the entries
	// it pointed at — so the high-water mark is carried over while the branch
	// starts fresh.
	at, err := s.appendPoint(ctx)
	if err != nil {
		return err
	}
	rows, err := s.encodeEntries(ctx, entries, agents.AppendPoint{LastSeq: at.LastSeq})
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
func (s *Session) encodeEntries(ctx context.Context, entries []agents.SessionEntry, at agents.AppendPoint) ([]entry, error) {
	prepared := agents.PrepareAppend(entries, at)
	rows := make([]entry, 0, len(prepared))
	for _, e := range prepared {
		data, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("encoding session entry: %w", err)
		}
		rows = append(rows, entry{
			SessionID: s.sessionID,
			Seq:       e.Seq,
			EntryID:   e.ID,
			ParentID:  e.ParentID,
			Kind:      string(e.Kind),
			Entry:     string(data),
		})
	}
	return rows, nil
}

// leaf reports the session's current branch tip, for linking an append.
//
// Only the last row is read: the tip is either that entry, or — when it is a
// leaf move — the entry it points at. Folding the whole session to learn the
// same thing would make every append cost a full read.
func (s *Session) appendPoint(ctx context.Context) (agents.AppendPoint, error) {
	var row entry
	err := s.db.NewSelect().Model(&row).
		Where("session_id = ?", s.sessionID).
		Order("seq DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return agents.AppendPoint{}, nil
	}
	if err != nil {
		return agents.AppendPoint{}, err
	}
	e, derr := decodeEntry(row)
	if derr != nil {
		return agents.AppendPoint{}, derr
	}
	// The newest row answers both questions: it carries the highest sequence
	// number this session has issued, and the tip is either it or — when it is
	// a leaf move — the entry it points at.
	return agents.AppendPoint{
		Leaf:    agents.LeafOf([]agents.SessionEntry{e}),
		LastSeq: row.Seq,
	}, nil
}

func decodeEntry(r entry) (agents.SessionEntry, error) {
	var e agents.SessionEntry
	if err := json.Unmarshal([]byte(r.Entry), &e); err != nil {
		return agents.SessionEntry{}, fmt.Errorf("decoding session entry %d: %w", r.ID, err)
	}
	return e, nil
}

var (
	_ agents.SessionStorage = (*Session)(nil)
	_ agents.AtomicReplacer = (*Session)(nil)
	_ agents.EntryPopper    = (*Session)(nil)
)

// Metadata implements agents.SessionStorage. It counts rather than loading, and
// merges in the session row when one exists (a session created through a repo).
func (s *Session) Metadata(ctx context.Context) (agents.SessionMetadata, error) {
	n, err := s.db.NewSelect().Model((*entry)(nil)).Where("session_id = ?", s.sessionID).Count(ctx)
	if err != nil {
		return agents.SessionMetadata{}, err
	}
	md := agents.SessionMetadata{ID: s.sessionID, EntryCount: n}

	var row sessionRow
	err = s.db.NewSelect().Model(&row).Where("id = ?", s.sessionID).Limit(1).Scan(ctx)
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

// Entry implements agents.SessionStorage with an indexed lookup rather than a
// scan of the session.
func (s *Session) Entry(ctx context.Context, id string) (*agents.SessionEntry, error) {
	var row entry
	err := s.db.NewSelect().Model(&row).
		Where("session_id = ?", s.sessionID).
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
