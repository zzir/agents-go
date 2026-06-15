// Package sessions provides SQL-backed agents.Session implementations (SQLite
// and PostgreSQL) built on uptrace/bun. It is a separate Go module so the
// database driver dependencies never reach the core SDK's dependency graph —
// callers who use only InMemorySession or memory.FileSession pay nothing for it.
//
// Conversation items are stored one row per item, serialized with
// agents.MarshalInputItem (and read back with agents.UnmarshalInputItem) so the
// Responses item encoding round-trips exactly as it does for FileSession.
package sessions

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/sqliteshim"

	"github.com/zzir/agents-go/agents"
)

// message is the row model: one stored conversation item, ordered by the
// autoincrement id within a session.
type message struct {
	bun.BaseModel `bun:"table:agent_messages,alias:m"`

	ID        int64  `bun:"id,pk,autoincrement"`
	SessionID string `bun:"session_id,notnull"`
	Item      string `bun:"item,notnull"` // JSON of agents.TResponseInputItem
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

// CreateSchema creates the agent_messages table and its lookup index if they do
// not already exist. It is safe to call repeatedly.
func CreateSchema(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*message)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	_, err := db.NewCreateIndex().
		Model((*message)(nil)).
		Index("idx_agent_messages_session").
		Column("session_id", "id").
		IfNotExists().
		Exec(ctx)
	return err
}

// GetItems implements agents.Session. A limit <= 0 returns all items
// oldest-first; a positive limit returns the most recent `limit` items, still
// oldest-first.
func (s *Session) GetItems(ctx context.Context, limit int) ([]agents.TResponseInputItem, error) {
	var rows []message
	q := s.db.NewSelect().Model(&rows).Where("session_id = ?", s.sessionID)
	if limit > 0 {
		q = q.Order("id DESC").Limit(limit)
	} else {
		q = q.Order("id ASC")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	if limit > 0 {
		slices.Reverse(rows) // most-recent-first -> oldest-first
	}
	items := make([]agents.TResponseInputItem, 0, len(rows))
	for _, r := range rows {
		item, err := agents.UnmarshalInputItem([]byte(r.Item))
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// AddItems implements agents.Session.
func (s *Session) AddItems(ctx context.Context, items []agents.TResponseInputItem) error {
	if len(items) == 0 {
		return nil
	}
	rows := make([]message, 0, len(items))
	for _, item := range items {
		data, err := agents.MarshalInputItem(item)
		if err != nil {
			return err
		}
		rows = append(rows, message{SessionID: s.sessionID, Item: string(data)})
	}
	_, err := s.db.NewInsert().Model(&rows).Exec(ctx)
	return err
}

// PopItem implements agents.Session, removing and returning the most recent
// item (or nil if the session is empty). The select-and-delete runs in one
// transaction so concurrent pops do not return the same row twice.
func (s *Session) PopItem(ctx context.Context) (*agents.TResponseInputItem, error) {
	var row message
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().Model(&row).
			Where("session_id = ?", s.sessionID).
			Order("id DESC").Limit(1).Scan(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model(&row).WherePK().Exec(ctx)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item, err := agents.UnmarshalInputItem([]byte(row.Item))
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// Clear implements agents.Session, removing every item for this session ID.
func (s *Session) Clear(ctx context.Context) error {
	_, err := s.db.NewDelete().Model((*message)(nil)).Where("session_id = ?", s.sessionID).Exec(ctx)
	return err
}

var _ agents.Session = (*Session)(nil)
