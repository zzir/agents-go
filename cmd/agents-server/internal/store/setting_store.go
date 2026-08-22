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
	// isSecret says which keys hold credentials, sealed at rest (SealIf). The
	// registry that knows lives in the settings package, which imports this
	// one — so it is handed in rather than imported.
	isSecret func(key string) bool
}

// NewSettingStore returns a SettingStore backed by db.
func NewSettingStore(db *bun.DB) *SettingStore {
	return &SettingStore{db: db}
}

// SealIf names the keys whose values are sealed at rest.
func (s *SettingStore) SealIf(isSecret func(key string) bool) { s.isSecret = isSecret }

func (s *SettingStore) seal(key, value string) string {
	if s.isSecret != nil && s.isSecret(key) {
		return sealSecret(labelSetting, value)
	}
	return value
}

func (s *SettingStore) open(st *Setting) (err error) {
	st.Value, err = openSecret(labelSetting, st.Value)
	return err
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
	if err := s.open(st); err != nil {
		return nil, fmt.Errorf("getting setting %s: %w", key, err)
	}
	return st, nil
}

// Set inserts or updates the value for key (upsert on key conflict).
func (s *SettingStore) Set(ctx context.Context, key, value string) error {
	st := &Setting{Key: key, Value: s.seal(key, value)}
	if _, err := s.db.NewInsert().Model(st).
		On("CONFLICT (key) DO UPDATE").
		Set("value = EXCLUDED.value").
		Exec(ctx); err != nil {
		return fmt.Errorf("setting %s: %w", key, err)
	}
	return nil
}

// List returns all settings ordered by key — all but the secret key check,
// which is the process's, not a setting.
func (s *SettingStore) List(ctx context.Context) ([]Setting, error) {
	var settings []Setting
	if err := s.db.NewSelect().Model(&settings).
		Where("key != ?", secretKeyCheck).
		OrderExpr("key ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing settings: %w", err)
	}
	for i := range settings {
		if err := s.open(&settings[i]); err != nil {
			return nil, fmt.Errorf("listing settings: %w", err)
		}
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
