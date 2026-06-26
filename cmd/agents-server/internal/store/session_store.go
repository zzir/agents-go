package store

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// SessionStore persists sessions and cascades deletes to their messages.
type SessionStore struct {
	db *bun.DB
}

// NewSessionStore returns a SessionStore backed by db.
func NewSessionStore(db *bun.DB) *SessionStore {
	return &SessionStore{db: db}
}

// Create inserts sess, stamping its created_at and updated_at timestamps.
func (s *SessionStore) Create(ctx context.Context, sess *Session) error {
	now := time.Now().UTC()
	sess.CreatedAt = now
	sess.UpdatedAt = now
	if _, err := s.db.NewInsert().Model(sess).Exec(ctx); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// List returns all sessions ordered newest first.
func (s *SessionStore) List(ctx context.Context) ([]Session, error) {
	var sessions []Session
	if err := s.db.NewSelect().Model(&sessions).
		OrderExpr("created_at DESC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return sessions, nil
}

// Get returns the session with the given id.
func (s *SessionStore) Get(ctx context.Context, id string) (*Session, error) {
	sess := new(Session)
	if err := s.db.NewSelect().Model(sess).
		Where("id = ?", id).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("getting session %s: %w", id, err)
	}
	return sess, nil
}

// Update renames the session with the given id and refreshes its updated_at.
func (s *SessionStore) Update(ctx context.Context, id string, name string) error {
	if _, err := s.db.NewUpdate().Model((*Session)(nil)).
		Set("name = ?", name).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx); err != nil {
		return fmt.Errorf("updating session %s: %w", id, err)
	}
	return nil
}

// SetPinned updates the pinned flag of the session with the given id.
func (s *SessionStore) SetPinned(ctx context.Context, id string, pinned bool) error {
	if _, err := s.db.NewUpdate().Model((*Session)(nil)).
		Set("pinned = ?", pinned).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx); err != nil {
		return fmt.Errorf("pinning session %s: %w", id, err)
	}
	return nil
}

// Delete removes the session with the given id and all of its messages in one
// transaction.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*Message)(nil)).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting messages for session %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*Session)(nil)).
			Where("id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting session %s: %w", id, err)
		}
		return nil
	})
}
