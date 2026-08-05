package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
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
	if sess.Gen == "" {
		// Assigned here rather than by each caller, so no path that creates a
		// session can forget it and leave one sharing its entries with whatever
		// held the name before.
		gen, err := session.NewGeneration()
		if err != nil {
			return err
		}
		sess.Gen = gen
	}
	now := time.Now().UTC()
	sess.CreatedAt = now
	sess.UpdatedAt = now
	if _, err := s.db.NewInsert().Model(sess).Exec(ctx); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// List returns all chat sessions, most recently CHANGED first — an append, a
// pop or a clear moves a session up exactly as a rename does (the entry store
// stamps updated_at on every write). Hidden task-transcript sessions (owned by
// a tasks row) are excluded — they surface through the parent session's task
// list, not the sidebar.
func (s *SessionStore) List(ctx context.Context) ([]Session, error) {
	var sessions []Session
	if err := s.db.NewSelect().Model(&sessions).
		Where("hidden = ?", false).
		OrderExpr("updated_at DESC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return sessions, nil
}

// Get returns the session with the given id, or an ErrNotFound-wrapping error
// when it doesn't exist.
func (s *SessionStore) Get(ctx context.Context, id string) (*Session, error) {
	sess := new(Session)
	if err := s.db.NewSelect().Model(sess).
		Where("id = ?", id).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting session %s: %w", id, err)
	}
	return sess, nil
}

// Update renames the session with the given id and refreshes its updated_at.
func (s *SessionStore) Update(ctx context.Context, id string, name string) error {
	np := &name
	return s.UpdateFields(ctx, id, np, nil)
}

// UpdateFields applies a partial update to the session: only non-nil fields
// are written; updated_at is always refreshed. Returns an ErrNotFound-wrapping
// error when the session doesn't exist.
func (s *SessionStore) UpdateFields(ctx context.Context, id string, name *string, pinned *bool) error {
	q := s.db.NewUpdate().Model((*Session)(nil)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id)
	if name != nil {
		q = q.Set("name = ?", *name)
	}
	if pinned != nil {
		q = q.Set("pinned = ?", *pinned)
	}
	res, err := q.Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("updating session %s: %w", id, err)
	}
	return nil
}

// Delete removes the session with the given id together with all of its
// messages, trace events, and pending approvals in one transaction. Background
// tasks spawned from the session cascade: their rows and hidden child
// sessions (with all their data) go too — a hidden session has no UI path of
// its own, so anything left behind would be unreachable forever.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var childIDs []string
		if err := tx.NewSelect().Model((*Task)(nil)).
			Column("child_session_id").
			Where("parent_session_id = ?", id).
			Scan(ctx, &childIDs); err != nil {
			return fmt.Errorf("listing task sessions for %s: %w", id, err)
		}
		for _, child := range childIDs {
			for _, model := range []any{(*entryRow)(nil), (*TraceEvent)(nil), (*PendingApproval)(nil)} {
				if _, err := tx.NewDelete().Model(model).
					Where("session_id = ?", child).
					Exec(ctx); err != nil {
					return fmt.Errorf("deleting task session %s data: %w", child, err)
				}
			}
			if _, err := tx.NewDelete().Model((*Session)(nil)).
				Where("id = ?", child).
				Exec(ctx); err != nil {
				return fmt.Errorf("deleting task session %s: %w", child, err)
			}
		}
		if _, err := tx.NewDelete().Model((*Task)(nil)).
			Where("parent_session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting tasks for session %s: %w", id, err)
		}
		// If id is itself a task's hidden child session (deleting it directly,
		// e.g. via the REST endpoint), drop the owning task row too — otherwise
		// its child_session_id dangles at a deleted session forever.
		if _, err := tx.NewDelete().Model((*Task)(nil)).
			Where("child_session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting owning task for session %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*entryRow)(nil)).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting entries for session %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*TraceEvent)(nil)).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting trace events for session %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*PendingApproval)(nil)).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting pending approvals for session %s: %w", id, err)
		}
		res, err := tx.NewDelete().Model((*Session)(nil)).
			Where("id = ?", id).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		if err != nil {
			return fmt.Errorf("deleting session %s: %w", id, err)
		}
		return nil
	})
}
