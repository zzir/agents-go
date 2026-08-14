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

// BindAgentIfEmpty records which agent config a session ran with, unless it is
// already bound to one: the first binding stands, so a later run with a
// different agent does not silently rewrite what a reload reopens the session
// with.
//
// A session that is already bound, or is not there at all, changes nothing and
// is not an error — this is a back-fill, and the caller has a run to finish
// either way. It leaves updated_at alone: the conversation did not change, and
// a listing sorts by that.
func (s *SessionStore) BindAgentIfEmpty(ctx context.Context, id, agentConfigID string) error {
	if agentConfigID == "" {
		return nil
	}
	if _, err := s.db.NewUpdate().Model((*Session)(nil)).
		Set("agent_config_id = ?", agentConfigID).
		Where("id = ?", id).
		Where("agent_config_id = '' OR agent_config_id IS NULL").
		Exec(ctx); err != nil {
		return fmt.Errorf("binding session %s to agent config %s: %w", id, agentConfigID, err)
	}
	return nil
}

// BindSandboxIfEmpty permanently binds (sandbox_id, work_dir) to the session,
// unless it is already bound: the first sandbox-carrying run wins and the
// binding is never rewritten — the session's file system context must not
// change under a conversation that already touched it. It reports whether THIS
// call performed the bind, so the winner (and only the winner) can announce
// it; a caller that lost re-reads the session to adopt the standing values.
//
// The EXISTS predicate makes "the target is still what was validated" part of
// the same atomic statement, so a concurrent delete or update cannot bind the
// session to a vanished config or a stale workdir. Matching the REVISION (not
// just the id) closes the update half; losing reads as won=false and the caller
// re-plans.
//
// An empty sandboxID binds nothing (a run without a sandbox leaves the session
// bindable later). A missing session is (false, nil) — the caller's own
// existence check owns that error. Like BindAgentIfEmpty, it leaves updated_at
// alone: the conversation did not change.
func (s *SessionStore) BindSandboxIfEmpty(ctx context.Context, id, sandboxID, workDir string, revision int64) (bool, error) {
	if sandboxID == "" {
		return false, nil
	}
	res, err := s.db.NewUpdate().Model((*Session)(nil)).
		Set("sandbox_id = ?", sandboxID).
		Set("work_dir = ?", workDir).
		Where("id = ?", id).
		Where("sandbox_id = '' OR sandbox_id IS NULL").
		Where("EXISTS (SELECT 1 FROM sandbox_configs WHERE id = ? AND revision = ?)", sandboxID, revision).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("binding session %s to sandbox %s: %w", id, sandboxID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("binding session %s to sandbox %s: %w", id, sandboxID, err)
	}
	return n > 0, nil
}

// CountBindingRefs reports how many sessions are bound to exactly (sandboxID,
// workDir) — the unit the SandboxManager caches an instance per. Zero after a
// session delete means the pair's cached instance has no caller left.
func (s *SessionStore) CountBindingRefs(ctx context.Context, sandboxID, workDir string) (int, error) {
	n, err := s.db.NewSelect().Model((*Session)(nil)).
		Where("sandbox_id = ?", sandboxID).
		Where("work_dir = ?", workDir).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting sessions bound to sandbox %s workdir %q: %w", sandboxID, workDir, err)
	}
	return n, nil
}

// Delete removes the session with the given id together with all of its
// messages, trace events, and pending approvals in one transaction. Background
// tasks spawned from the session cascade: their rows and hidden child
// sessions (with all their data) go too — a hidden session has no UI path of
// its own, so anything left behind would be unreachable forever.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Every hidden session this one owns: a task's and a workflow's alike.
		// Both are unreachable once their owner is gone.
		var childIDs []string
		if err := tx.NewSelect().Model((*Task)(nil)).
			Column("child_session_id").
			Where("parent_session_id = ?", id).
			Scan(ctx, &childIDs); err != nil {
			return fmt.Errorf("listing task sessions for %s: %w", id, err)
		}
		var workflowChildren []string
		if err := tx.NewSelect().Model((*WorkflowRun)(nil)).
			Column("child_session_id").
			Where("parent_session_id = ?", id).
			Scan(ctx, &workflowChildren); err != nil {
			return fmt.Errorf("listing workflow sessions for %s: %w", id, err)
		}
		for _, child := range workflowChildren {
			if child != "" {
				childIDs = append(childIDs, child)
			}
		}
		for _, child := range childIDs {
			for _, model := range []any{(*entryRow)(nil), (*appendPointRow)(nil), (*TraceEvent)(nil), (*PendingApproval)(nil), (*ContextProfile)(nil), (*Wakeup)(nil)} {
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
		if _, err := tx.NewDelete().Model((*appendPointRow)(nil)).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting the append point for session %s: %w", id, err)
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
		if _, err := tx.NewDelete().Model((*ContextProfile)(nil)).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting the context profile for session %s: %w", id, err)
		}
		// Both ends: a deleted chat takes its executions with it, and a deleted
		// child session (a workflow's own) leaves no row pointing at nothing.
		if _, err := tx.NewDelete().Model((*WorkflowRun)(nil)).
			Where("parent_session_id = ? OR child_session_id = ?", id, id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting workflow runs for session %s: %w", id, err)
		}
		if _, err := tx.NewDelete().Model((*Wakeup)(nil)).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting wake-ups for session %s: %w", id, err)
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
