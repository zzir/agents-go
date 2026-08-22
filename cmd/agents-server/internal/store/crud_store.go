package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// NewID returns a random 128-bit hex identifier, used as a primary key for
// stored entities.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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
