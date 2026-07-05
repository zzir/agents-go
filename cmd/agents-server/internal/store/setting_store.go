package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// SettingStore persists key/value server settings.
type SettingStore struct {
	db *bun.DB
}

// NewSettingStore returns a SettingStore backed by db.
func NewSettingStore(db *bun.DB) *SettingStore {
	return &SettingStore{db: db}
}

// Get returns the setting with the given key, or an ErrNotFound-wrapping
// error when it doesn't exist.
func (s *SettingStore) Get(ctx context.Context, key string) (*Setting, error) {
	st := new(Setting)
	if err := s.db.NewSelect().Model(st).
		Where("key = ?", key).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting setting %s: %w", key, err)
	}
	return st, nil
}

// Set inserts or updates the value for key (upsert on key conflict).
func (s *SettingStore) Set(ctx context.Context, key, value string) error {
	st := &Setting{Key: key, Value: value}
	if _, err := s.db.NewInsert().Model(st).
		On("CONFLICT (key) DO UPDATE").
		Set("value = EXCLUDED.value").
		Exec(ctx); err != nil {
		return fmt.Errorf("setting %s: %w", key, err)
	}
	return nil
}

// List returns all settings ordered by key.
func (s *SettingStore) List(ctx context.Context) ([]Setting, error) {
	var settings []Setting
	if err := s.db.NewSelect().Model(&settings).
		OrderExpr("key ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing settings: %w", err)
	}
	return settings, nil
}

// Delete removes the setting with the given key. Returns an
// ErrNotFound-wrapping error when it doesn't exist.
func (s *SettingStore) Delete(ctx context.Context, key string) error {
	res, err := s.db.NewDelete().Model((*Setting)(nil)).
		Where("key = ?", key).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("deleting setting %s: %w", key, err)
	}
	return nil
}
