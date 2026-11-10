package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
)

// NewID returns a UUIDv4 — the primary key of ordinary entities (README
// "Database"). Secrets are NOT ids and keep 256-bit crypto/rand.
func NewID() string {
	return uuid.NewV4().String()
}

// IsID reports whether s is an id as NewID and NewTimeID spell one. No row is
// keyed by anything else, so a lookup by it is a miss on both dialects.
func IsID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// DecodeConfig decodes a stored Config payload into v; an empty payload is
// the zero config. Unknown keys are ignored.
func DecodeConfig(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// NewTimeID returns a UUIDv7 — for the append-heavy tables whose ids double
// as pagination cursors (docs/howto/workbench-deploy.md "Database").
func NewTimeID() string {
	return uuid.NewV7().String()
}

// uuidOrNull is an empty id as a raw-SQL bind value: NULL, which a uuid
// column accepts where "" is a syntax error (a Set("col = ?") gets no nullzero).
func uuidOrNull(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// stampOnAppend is the shared BeforeAppendModel logic: an ID and timestamps
// on insert, a refreshed updated_at on update.
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

// ErrRevisionConflict reports an update whose expected revision no longer
// matches the row: another update landed in between. Handlers map it to 409.
var ErrRevisionConflict = errors.New("the record changed concurrently; re-read and retry")

// CrudStore is a generic store for entities keyed by a string "id" primary
// key with created_at/updated_at columns; entity stores embed it and export
// one of its writes only as a forwarder (invariant 103).
type CrudStore[T any] struct {
	db    *bun.DB
	label string // human-readable name for error messages, e.g. "agent config"
	// seal/open transform the entity's credential fields around the database
	// (withSecrets); the caller's value is plaintext before and after every call.
	seal func(*T) error
	open func(*T) error
}

// NewCrudStore returns a CrudStore for T using label in error messages.
func NewCrudStore[T any](db *bun.DB, label string) *CrudStore[T] {
	return &CrudStore[T]{db: db, label: label}
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

// sealedWrite runs exec with m's credentials sealed, then opens them again
// whatever exec returned, so the caller keeps a plaintext struct.
func (s *CrudStore[T]) sealedWrite(m *T, exec func() error) error {
	if err := s.sealed(m); err != nil {
		return err
	}
	err := exec()
	if oerr := s.opened(m); err == nil {
		err = oerr
	}
	return err
}

// create inserts m as a new row.
func (s *CrudStore[T]) create(ctx context.Context, m *T) error {
	err := s.sealedWrite(m, func() error {
		_, err := s.db.NewInsert().Model(m).Exec(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("creating %s: %w", s.label, err)
	}
	return nil
}

// List returns all rows, newest first.
func (s *CrudStore[T]) List(ctx context.Context) ([]T, error) {
	var out []T
	if err := s.db.NewSelect().Model(&out).OrderExpr("created_at DESC").Scan(ctx); err != nil {
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
	m, err := getByID[T](ctx, s.db, s.label, id)
	if err != nil {
		return nil, err
	}
	if err := s.opened(m); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", s.label, id, err)
	}
	return m, nil
}

// getByID reads the row of T keyed by id; a miss, or an id no row is keyed
// by (IsID), is an ErrNotFound-wrapping error.
func getByID[T any](ctx context.Context, db bun.IDB, label, id string) (*T, error) {
	if !IsID(id) {
		return nil, fmt.Errorf("getting %s %s: %w", label, id, ErrNotFound)
	}
	m := new(T)
	if err := db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting %s %s: %w", label, id, err)
	}
	return m, nil
}

// update overwrites every column of the row except id and created_at.
// Returns an ErrNotFound-wrapping error when the row doesn't exist.
func (s *CrudStore[T]) update(ctx context.Context, id string, m *T) error {
	err := s.sealedWrite(m, func() error {
		res, err := s.db.NewUpdate().Model(m).
			ExcludeColumn("id", "created_at").
			Where("id = ?", id).
			Exec(ctx)
		if err != nil {
			return err
		}
		return requireRows(res)
	})
	if err != nil {
		return fmt.Errorf("updating %s %s: %w", s.label, id, err)
	}
	return nil
}

// lockRow reads the row matching where into model inside tx — SELECT ... FOR
// UPDATE on PostgreSQL; SQLite's one connection serializes by itself.
// ErrNotFound when none.
func lockRow(ctx context.Context, tx bun.Tx, model any, where string, arg any) error {
	return readLocked(ctx, tx, model, "UPDATE", where, arg)
}

// shareRow is lockRow for a row the transaction only references — FOR SHARE:
// referencing writes do not wait on each other, the row's own writers do
// (invariant 102).
func shareRow(ctx context.Context, tx bun.Tx, model any, where string, arg any) error {
	return readLocked(ctx, tx, model, "SHARE", where, arg)
}

func readLocked(ctx context.Context, tx bun.Tx, model any, strength, where string, arg any) error {
	if err := rowLock(tx.NewSelect().Model(model).Where(where, arg), strength).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// rowLock makes q a locking read on PostgreSQL (FOR strength); SQLite's one
// connection serializes by itself.
func rowLock(q *bun.SelectQuery, strength string) *bun.SelectQuery {
	if q.Dialect().Name() == dialect.PG {
		return q.For(strength)
	}
	return q
}

// updateFrom is the read-modify-write behind every credential-keeping Update:
// read the row locked, hand it to prepare, overwrite every column but id,
// created_at and keep.
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
	return s.sealedWrite(m, func() error {
		res, err := tx.NewUpdate().Model(m).
			ExcludeColumn(append([]string{"id", "created_at"}, keep...)...).
			Where("id = ?", id).
			Exec(ctx)
		if err != nil {
			return err
		}
		return requireRows(res)
	})
}

// updateInTx is updateFrom in a transaction of its own: the whole Update of
// an entity with nothing else to write beside the row.
func (s *CrudStore[T]) updateInTx(ctx context.Context, id string, m *T, prepare func(prev *T) error, keep ...string) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return s.updateFrom(ctx, tx, id, m, prepare, keep...)
	})
	if err != nil {
		return fmt.Errorf("updating %s %s: %w", s.label, id, err)
	}
	return nil
}

// staleOrMissing explains a compare-and-set on revision that matched no row:
// the row is gone (ErrNotFound) or its revision moved (ErrRevisionConflict).
func (s *CrudStore[T]) staleOrMissing(ctx context.Context, id string) error {
	exists, err := s.db.NewSelect().Model((*T)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return fmt.Errorf("updating %s %s: %w", s.label, id, err)
	}
	if !exists {
		return fmt.Errorf("updating %s %s: %w", s.label, id, ErrNotFound)
	}
	return ErrRevisionConflict
}

// delete removes the row with the given id. Returns an ErrNotFound-wrapping
// error when the row doesn't exist.
func (s *CrudStore[T]) delete(ctx context.Context, id string) error {
	res, err := s.db.NewDelete().Model((*T)(nil)).Where("id = ?", id).Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("deleting %s %s: %w", s.label, id, err)
	}
	return nil
}

// pruneBatchSize bounds one DELETE of a maintenance sweep (SQLite has one connection).
var pruneBatchSize = 5000

// deleteInBatches deletes the rows of model that match where, batch by
// batch until none match, and returns the count.
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
