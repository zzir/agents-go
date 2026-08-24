package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
)

// NewID returns a UUIDv4 — the primary key of ordinary entities, whose row
// counts stay small enough that index locality buys nothing. Secrets —
// tokens, states, the webhook secret — are NOT ids and keep 256-bit
// crypto/rand.
func NewID() string {
	return uuid.NewV4().String()
}

// NewTimeID returns a UUIDv7 — for the APPEND-HEAVY tables only
// (trace_events, entries, audit_events), where time-ordered ids keep inserts
// at the right edge of the B-tree index and rows made together sitting
// together instead of churning random pages as the table grows.
func NewTimeID() string {
	return uuid.NewV7().String()
}

// uuidOrNull is an empty id as a raw-SQL bind value: NULL, which a uuid
// column accepts where "" is a syntax error. Model writes get this from the
// nullzero tag; a Set("col = ?") does not.
func uuidOrNull(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// stampOnAppend is the shared BeforeAppendModel logic for entities with a
// string "id" primary key and created_at/updated_at columns: it assigns an ID
// and timestamps on insert and refreshes updated_at on update. Each such model
// implements BeforeAppendModel by delegating here.
func stampOnAppend(query bun.Query, id *string, createdAt, updatedAt *time.Time) error {
	switch query.(type) {
	case *bun.InsertQuery:
		if *id == "" {
			*id = NewID()
		}
		now := time.Now().UTC()
		if createdAt.IsZero() {
			*createdAt = now
		}
		*updatedAt = now
	case *bun.UpdateQuery:
		*updatedAt = time.Now().UTC()
	}
	return nil
}

// CrudStore is a generic store for entities keyed by a string "id" primary key
// with created_at/updated_at columns. Embed it (e.g. AgentConfigStore) to get
// Create/List/Get/Update/Delete and add entity-specific queries alongside.
type CrudStore[T any] struct {
	db    *bun.DB
	label string // human-readable name for error messages, e.g. "agent config"
	order string // ORDER BY expression for List, e.g. "updated_at DESC"
	// seal/open transform the entity's credential fields around the database
	// (withSecrets). The caller's value is plaintext before and after every
	// call; only the row is sealed.
	seal func(*T) error
	open func(*T) error
}

// NewCrudStore returns a CrudStore for T using label in error messages and order
// as the List ORDER BY expression.
func NewCrudStore[T any](db *bun.DB, label, order string) *CrudStore[T] {
	return &CrudStore[T]{db: db, label: label, order: order}
}

// withSecrets installs the seal/open transforms for T's credential fields.
func (s *CrudStore[T]) withSecrets(seal, open func(*T) error) *CrudStore[T] {
	s.seal, s.open = seal, open
	return s
}

func (s *CrudStore[T]) sealed(m *T) error {
	if s.seal == nil {
		return nil
	}
	return s.seal(m)
}

func (s *CrudStore[T]) opened(m *T) error {
	if s.open == nil {
		return nil
	}
	return s.open(m)
}

// Create inserts m as a new row.
func (s *CrudStore[T]) Create(ctx context.Context, m *T) error {
	if err := s.sealed(m); err != nil {
		return fmt.Errorf("creating %s: %w", s.label, err)
	}
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if oerr := s.opened(m); err == nil {
		err = oerr
	}
	if err != nil {
		return fmt.Errorf("creating %s: %w", s.label, err)
	}
	return nil
}

// List returns all rows ordered by the store's configured ORDER BY expression.
func (s *CrudStore[T]) List(ctx context.Context) ([]T, error) {
	var out []T
	q := s.db.NewSelect().Model(&out)
	if s.order != "" {
		q = q.OrderExpr(s.order)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing %s: %w", s.label, err)
	}
	for i := range out {
		if err := s.opened(&out[i]); err != nil {
			return nil, fmt.Errorf("listing %s: %w", s.label, err)
		}
	}
	return out, nil
}

// Get returns the row with the given id, or an ErrNotFound-wrapping error
// when it doesn't exist.
func (s *CrudStore[T]) Get(ctx context.Context, id string) (*T, error) {
	m := new(T)
	if err := s.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting %s %s: %w", s.label, id, err)
	}
	if err := s.opened(m); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", s.label, id, err)
	}
	return m, nil
}

// Update overwrites every column of the row except the immutable id and
// created_at; updated_at is refreshed by the model's BeforeAppendModel hook.
// Returns an ErrNotFound-wrapping error when the row doesn't exist.
func (s *CrudStore[T]) Update(ctx context.Context, id string, m *T) error {
	if err := s.sealed(m); err != nil {
		return fmt.Errorf("updating %s %s: %w", s.label, id, err)
	}
	res, err := s.db.NewUpdate().Model(m).
		ExcludeColumn("id", "created_at").
		Where("id = ?", id).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if oerr := s.opened(m); err == nil {
		err = oerr
	}
	if err != nil {
		return fmt.Errorf("updating %s %s: %w", s.label, id, err)
	}
	return nil
}

// lockRow reads the row matching where into model inside tx, for a
// read-modify-write. On PostgreSQL it is SELECT ... FOR UPDATE, so a
// concurrent write of the same row waits instead of landing over a stale
// read; SQLite's one connection serializes by itself. ErrNotFound when no
// row matches.
func lockRow(ctx context.Context, tx bun.Tx, model any, where string, arg any) error {
	q := tx.NewSelect().Model(model).Where(where, arg)
	if tx.Dialect().Name() == dialect.PG {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// updateFrom is the read-modify-write behind the Update of every store that
// keeps a credential across edits: inside tx, read the row (locked), hand it
// to prepare — which folds into m what m keeps from it — then overwrite every
// column but id, created_at and keep. A nil prepare just writes m.
func (s *CrudStore[T]) updateFrom(ctx context.Context, tx bun.Tx, id string, m *T, prepare func(prev *T) error, keep ...string) error {
	prev := new(T)
	if err := lockRow(ctx, tx, prev, "id = ?", id); err != nil {
		return err
	}
	if err := s.opened(prev); err != nil {
		return err
	}
	if prepare != nil {
		if err := prepare(prev); err != nil {
			return err
		}
	}
	if err := s.sealed(m); err != nil {
		return err
	}
	res, err := tx.NewUpdate().Model(m).
		ExcludeColumn(append([]string{"id", "created_at"}, keep...)...).
		Where("id = ?", id).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if oerr := s.opened(m); err == nil {
		err = oerr
	}
	return err
}

// Delete removes the row with the given id. Returns an ErrNotFound-wrapping
// error when the row doesn't exist.
func (s *CrudStore[T]) Delete(ctx context.Context, id string) error {
	res, err := s.db.NewDelete().Model((*T)(nil)).Where("id = ?", id).Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("deleting %s %s: %w", s.label, id, err)
	}
	return nil
}

// pruneBatchSize bounds one DELETE of a maintenance sweep: on SQLite the
// pool is one connection, so a sweep that deleted a month of rows in one
// statement would hold every append and read for its whole duration.
var pruneBatchSize = 5000

// deleteInBatches deletes the rows of model that match where, batch by
// batch until none match, and returns the count. where scopes a SELECT of
// ids; the DELETE targets those ids.
func deleteInBatches(ctx context.Context, db *bun.DB, model any, where func(*bun.SelectQuery) *bun.SelectQuery) (int64, error) {
	var total int64
	for {
		ids := where(db.NewSelect().Model(model).Column("id")).Limit(pruneBatchSize)
		res, err := db.NewDelete().Model(model).Where("id IN (?)", ids).Exec(ctx)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(pruneBatchSize) {
			return total, nil
		}
	}
}
