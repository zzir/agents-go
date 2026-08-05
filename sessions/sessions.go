// Package sessions provides SQL-backed session.Storage implementations (SQLite
// and PostgreSQL) built on uptrace/bun. It is a separate Go module so the
// database driver dependencies never reach the core SDK's dependency graph —
// callers who use only InMemorySession or filesession.Store pay nothing for it.
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
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"

	"github.com/zzir/agents-go/agents/session"
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
	// Gen is the session generation these entries belong to; see
	// session.Ref. Empty is the direct scope, which is a scope like any
	// other and not a wildcard.
	Gen string `bun:"gen,notnull"`
	// Seq is the entry's cursor position, allocated by session.PrepareAppend.
	//
	// It is a column rather than the row's autoincrement id because the two are
	// not the same number: an id is unique per TABLE and assigned on insert,
	// while Seq is the session-local position a Cursor pages on and has to
	// survive a fork or an export that carries entries between stores.
	Seq      int64  `bun:"seq,notnull"`
	EntryID  string `bun:"entry_id,notnull"`
	ParentID string `bun:"parent_id"`
	Kind     string `bun:"kind,notnull"`
	Entry    string `bun:"entry,notnull"` // JSON of session.Entry
}

// sessionRow records a session's existence and metadata, so a repo can list
// sessions that hold no entries yet and so "hidden" is a property of the
// session rather than something inferred from its contents.
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

// scoped narrows a query to this session. Reads and writes both go through it,
// which is what makes the generation part of the address rather than a field a
// code path can forget.
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

// CreateSchema creates the agent_entries table and its lookup indexes if they
// do not already exist. It is safe to call repeatedly.
//
// Both indexes are UNIQUE, and that is load-bearing rather than an
// optimization: sequence numbers and entry ids are never handed out twice
// (spec §2.5e2), and a backend that can constrain them does — so a race or a
// bug that would mint a duplicate becomes a failed write instead of two rows
// answering to one name.
//
// The schema changed shape when sessions moved from items to entries; there is
// no migration from agent_messages, and none for the indexes turning unique.
// This project does not ship migrations — rebuild the database.
func CreateSchema(ctx context.Context, db *bun.DB) error {
	// The task table comes with it: Repo.Delete cascades task rows, so a
	// database created by this call alone must have somewhere for that delete
	// to look. CreateTaskSchema in turn creates the session table it resolves
	// generations against, so either entry point leaves a consistent schema
	// and calling both changes nothing (every creation is IfNotExists).
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
	_, err := db.NewCreateIndex().
		Model((*entry)(nil)).
		Index("idx_agent_entries_entry_id").
		Unique().
		Column("session_id", "gen", "entry_id").
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

// lockForWrite serializes this session's read-plan-write sequences.
//
// Reading the append point and writing the entries must be one step (spec
// §2.5e2): two writers that both read the same tip mint the same sequence
// numbers and fork the branch, silently. A transaction alone does not give
// that — under read committed both transactions still read the old tip — so:
//
//   - PostgreSQL takes a transaction-scoped advisory lock on (id, gen),
//     released automatically at commit or rollback.
//   - SQLite needs nothing here: NewSQLite caps the pool at one connection,
//     and a transaction holds it for its whole extent, so a competing write
//     cannot interleave. (A caller wiring their own multi-connection SQLite
//     pool through New gives that serialization up. The unique indexes still
//     catch a doubly-issued seq or id, but two appends against one tip with
//     distinct sequence numbers fork the branch without tripping anything —
//     cap the pool, as NewSQLite does.)
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

// touchIn records that the session changed, on the transaction that changed it
// — one step, so the record cannot exist without the write or the write
// without the record.
//
// For a repo-created session (non-empty generation) it doubles as the proof
// the session still EXISTS: zero rows updated means the row is gone — deleted
// under a live handle — and the write must fail and roll back rather than
// leave entries no session references, unreachable forever (spec §2.5e2:
// writing and proving the destination still exists are one step). A session
// used directly (empty generation) never had a row; for it, zero rows is
// simply nothing to record.
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

// PopEntry implements session.EntryPopper.
func (s *Session) PopEntry(ctx context.Context) (*session.Entry, error) {
	return s.pop(ctx, session.PopLast)
}

// PopItem implements session.ItemPopper.
func (s *Session) PopItem(ctx context.Context) (*session.Entry, error) {
	return s.pop(ctx, session.PopLastItem)
}

// pop selects the entry to remove, deletes it and applies its relinks all in
// ONE transaction, holding the write lock. Selecting on one view and deleting
// on another is how a concurrent append's child ends up hanging off an id that
// is gone — and a walk meeting a missing parent stops there, losing everything
// BEFORE the removed entry rather than just it.
//
// The delete still arbitrates: a writer not holding the lock (a foreign tool
// touching the same database) can take the row first, in which case zero rows
// are affected, this caller lost, and it retries against what the session
// holds now.
func (s *Session) pop(ctx context.Context, mode session.PopMode) (*session.Entry, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var popped *session.Entry
		done := true
		err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if err := s.lockForWrite(ctx, tx); err != nil {
				return err
			}
			entries, err := s.entriesIn(ctx, tx, session.Cursor{})
			if err != nil {
				return err
			}
			plan, ok := session.PlanPop(entries, mode)
			if !ok {
				return nil
			}
			res, derr := tx.NewDelete().Model((*entry)(nil)).
				Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
				Where("entry_id IN (?)", bun.List(plan.Delete)).Exec(ctx)
			if derr != nil {
				return derr
			}
			if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
				done = false
				return nil
			}
			byID := make(map[string]session.Entry, len(entries))
			for _, e := range entries {
				byID[e.ID] = e
			}
			if err := s.relinkIn(ctx, tx, plan, byID); err != nil {
				return err
			}
			if err := s.touchIn(ctx, tx); err != nil {
				return err
			}
			popped = &plan.Entry
			return nil
		})
		if err != nil {
			return nil, err
		}
		if done {
			return popped, nil
		}
	}
}

// relinkIn re-points the entries a removal orphaned, on the transaction that
// carried the delete.
func (s *Session) relinkIn(ctx context.Context, tx bun.Tx, plan session.Removal, byID map[string]session.Entry) error {
	for id, parent := range plan.Relink {
		e, ok := byID[id]
		if !ok {
			continue
		}
		if e.Kind == session.EntryKindLeaf {
			updated, lerr := e.WithLeafTarget(parent)
			if lerr != nil {
				continue
			}
			e = updated
		} else {
			e.ParentID = parent
		}
		raw, merr := json.Marshal(e)
		if merr != nil {
			return fmt.Errorf("encoding relinked entry %q: %w", id, merr)
		}
		if _, uerr := tx.NewUpdate().Model((*entry)(nil)).
			Set("parent_id = ?", e.ParentID).
			Set("entry = ?", string(raw)).
			Where("session_id = ?", s.ref.ID).Where("gen = ?", s.ref.Gen).
			Where("entry_id = ?", id).Exec(ctx); uerr != nil {
			return uerr
		}
	}
	return nil
}

// Clear implements session.Storage, removing every entry for this session ID.
// Clearing is a change like any other: it moves the session in a listing, and
// it holds the same write lock every other entry write holds — an unlocked
// clear interleaving with a locked append can otherwise land between the
// append's tip-read and its insert, leaving the new entry parented at a row
// the clear removed.
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

// replaceIn deletes this session's rows and inserts entries in their place on
// the caller's transaction, reporting whether it wrote. A non-nil expect makes
// the rewrite conditional on the session's highest sequence number still being
// that one.
func (s *Session) replaceIn(ctx context.Context, tx bun.Tx, entries []session.Entry, expect *int64) (bool, error) {
	if err := s.lockForWrite(ctx, tx); err != nil {
		return false, err
	}
	// A replace does not restart the numbering — a cursor outlives the
	// entries it pointed at — so the high-water mark is carried over while
	// the branch starts fresh.
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

// encodeEntries prepares entries for insertion, filling in the fields the store
// owns. A caller-supplied id is kept, so an entry re-added by a fork or a
// replace keeps the identity an update entry points at.
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

// appendPointIn reports the session's current branch tip, for linking an
// append, reading through the caller's transaction so tip and write are one
// step.
//
// Only the last row is read: the tip is either that entry, or — when it is a
// leaf move — the entry it points at. Folding the whole session to learn the
// same thing would make every append cost a full read.
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
	// The newest row answers both questions: it carries the highest sequence
	// number this session has issued, and the tip is either it or — when it is
	// a leaf move — the entry it points at.
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
	_ session.EntryPopper     = (*Session)(nil)
	_ session.ItemPopper      = (*Session)(nil)
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
