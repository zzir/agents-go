package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

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

// Create inserts sess, stamping its created_at and updated_at timestamps. An
// owner is required: a session nobody owns would be one nobody can open.
func (s *SessionStore) Create(ctx context.Context, sess *Session) error {
	if sess.OwnerID == "" {
		return errors.New("creating session: no owner")
	}
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

// EveryOwner is the owner argument that lists every owner's rows — the
// admin's explicit ask. An empty owner is refused, so a handler that forgot
// to resolve its caller lists nothing rather than everything.
const EveryOwner = "*"

var errListNoOwner = errors.New("listing without an owner")

// List returns one owner's chat sessions, most recently CHANGED first — an
// append, a pop or a clear moves a session up exactly as a rename does (the
// entry store stamps updated_at on every write). Hidden task-transcript
// sessions (owned by a tasks row) are excluded — they surface through the
// parent session's task list, not the sidebar. An empty ownerID lists every
// owner's: the admin's management view, never a member's sidebar.
func (s *SessionStore) List(ctx context.Context, ownerID string) ([]Session, error) {
	if ownerID == "" {
		return nil, errListNoOwner
	}
	var sessions []Session
	q := s.db.NewSelect().Model(&sessions).Where("hidden = ?", false)
	if ownerID != EveryOwner {
		q = q.Where("owner_id = ?", ownerID)
	}
	if err := q.OrderExpr("updated_at DESC").Scan(ctx); err != nil {
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

// DefaultSessionName is the name a session is created with until it is named
// — by the person, or by the title generator, which only ever names a session
// still carrying it.
const DefaultSessionName = "New Session"

// NameIfDefault sets the name only while the session still carries the
// default one — the CAS the title generator writes through, so a rename made
// while it was thinking stands, and of two generators only one lands. Reports
// whether the write took.
func (s *SessionStore) NameIfDefault(ctx context.Context, id, name string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*Session)(nil)).
		Set("name = ?", name).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("name = ?", DefaultSessionName).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("naming session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("naming session %s: %w", id, err)
	}
	return n > 0, nil
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
		Where("agent_config_id IS NULL").
		Exec(ctx); err != nil {
		return fmt.Errorf("binding session %s to agent config %s: %w", id, agentConfigID, err)
	}
	return nil
}

// BindProjectIfEmpty permanently binds project_id to the session unless it is
// already bound (decisions §5.28), reporting whether THIS call performed the
// bind so only the winner announces it. The project's existence is pinned
// inside the write: SQLite's single writer makes the EXISTS predicate atomic
// with the update; PostgreSQL locks the project row FOR KEY SHARE, against
// ProjectStore.DeleteIfUnreferenced's FOR UPDATE.
//
// An empty projectID binds nothing. A missing session is (false, nil) — the
// caller's own existence check owns that error. Like BindAgentIfEmpty, it
// leaves updated_at alone: the conversation did not change.
func (s *SessionStore) BindProjectIfEmpty(ctx context.Context, id, projectID string) (bool, error) {
	if projectID == "" {
		return false, nil
	}
	var res sql.Result
	var err error
	if s.db.Dialect().Name() == dialect.PG {
		err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var pid string
			lerr := tx.NewSelect().Model((*Project)(nil)).Column("id").
				Where("id = ?", projectID).For("KEY SHARE").Scan(ctx, &pid)
			if errors.Is(lerr, sql.ErrNoRows) {
				return nil // no project row: bind nothing (res stays nil)
			}
			if lerr != nil {
				return lerr
			}
			res, lerr = tx.NewUpdate().Model((*Session)(nil)).
				Set("project_id = ?", projectID).
				Where("id = ?", id).
				Where("project_id IS NULL").
				Exec(ctx)
			return lerr
		})
	} else {
		res, err = s.db.NewUpdate().Model((*Session)(nil)).
			Set("project_id = ?", projectID).
			Where("id = ?", id).
			Where("project_id IS NULL").
			Where("EXISTS (SELECT 1 FROM projects WHERE id = ?)", projectID).
			Exec(ctx)
	}
	if err != nil {
		return false, fmt.Errorf("binding session %s to project %s: %w", id, projectID, err)
	}
	if res == nil {
		return false, nil // the project vanished before the lock
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("binding session %s to project %s: %w", id, projectID, err)
	}
	return n > 0, nil
}

// CountProjectRefs reports how many sessions bind projectID — the unit the
// SandboxManager caches an instance per. Zero after a session delete means the
// project's cached instance has no caller left. An empty id counts the
// sessions that carry no binding, which the column stores as NULL (see
// boundTo).
func (s *SessionStore) CountProjectRefs(ctx context.Context, projectID string) (int, error) {
	n, err := boundTo(s.db.NewSelect().Model((*Session)(nil)), "project_id", projectID).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting sessions bound to project %s: %w", projectID, err)
	}
	return n, nil
}

// boundTo narrows a nullable uuid column to one id — or, for the empty id, to
// the rows that carry none. An unset binding is stored as NULL (the nullzero
// tag), never as "", and PostgreSQL refuses "" as a uuid outright; SQLite
// merely matches nothing, which reads as a passing test until it runs on the
// other backend.
func boundTo(q *bun.SelectQuery, column, id string) *bun.SelectQuery {
	if id == "" {
		return q.Where("? IS NULL", bun.Ident(column))
	}
	return q.Where("? = ?", bun.Ident(column), id)
}

// Delete removes the session with the given id together with all of its
// messages, trace events, pending approvals, wake-ups and triggers in one
// transaction. Background tasks spawned from the session cascade: their rows
// and hidden child sessions (with all their data) go too, the whole tree — a
// hidden session has no UI path of its own, so anything left behind would be
// unreachable forever. Only LIVE edges are followed (liveParent / liveChild,
// the fence every read applies): a stale task row from an earlier incarnation
// names a child id that may since have been given to an unrelated session,
// and following it would delete that session's history. The root's absence
// is ErrNotFound; a child that is already gone is not.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		tree, err := sessionTree(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, cur := range tree {
			if err := deleteSessionRows(ctx, tx, cur, cur == id); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetOwner reassigns the session and every hidden session serving it (its
// tasks' transcripts, recursively) to ownerID — the one ownership column
// stays consistent across the tree. ErrNotFound when the root is absent.
func (s *SessionStore) SetOwner(ctx context.Context, id, ownerID string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		tree, err := sessionTree(ctx, tx, id)
		if err != nil {
			return err
		}
		res, err := tx.NewUpdate().Model((*Session)(nil)).
			Set("owner_id = ?", ownerID).
			Where("id IN (?)", bun.List(tree)).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("reassigning session %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			return ErrNotFound
		}
		return err
	})
}

// sessionTree lists id and every hidden session serving it, parents before
// children, following the live task edges (a task row whose session was
// deleted and re-created is not an edge).
func sessionTree(ctx context.Context, tx bun.Tx, id string) ([]string, error) {
	tree := []string{id}
	visited := map[string]bool{id: true}
	for i := 0; i < len(tree); i++ {
		var childIDs []string
		if err := tx.NewSelect().Model((*Task)(nil)).
			Column("child_session_id").
			Where("parent_session_id = ?", tree[i]).
			Where(liveParent).Where(liveChild).
			Scan(ctx, &childIDs); err != nil {
			return nil, fmt.Errorf("listing task sessions for %s: %w", tree[i], err)
		}
		for _, c := range childIDs {
			if !visited[c] {
				visited[c] = true
				tree = append(tree, c)
			}
		}
	}
	return tree, nil
}

// deleteSessionRows removes one session of the tree: its row and everything
// keyed by its id, plus the task rows naming it in either role — as a PARENT
// (its own tasks) and as a CHILD (the task whose hidden session it is; a row
// pointing at a deleted session would dangle forever) — and the triggers that
// fire into it. mustExist makes a missing row ErrNotFound (the root); a child
// already gone is left as such.
func deleteSessionRows(ctx context.Context, tx bun.Tx, id string, mustExist bool) error {
	// The session row's lock first — the order every entry write takes
	// (EntryStore.lockSessionIn) — so an append and this cascade cannot each
	// wait on the other. A row already gone is what mustExist decides below.
	if tx.Dialect().Name() == dialect.PG {
		err := tx.NewSelect().Model((*Session)(nil)).Column("id").
			Where("id = ?", id).For("UPDATE").Scan(ctx, new(string))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("deleting session %s: %w", id, err)
		}
	}
	for _, model := range []any{(*entryRow)(nil), (*appendPointRow)(nil), (*TraceEvent)(nil), (*PendingApproval)(nil), (*ContextProfile)(nil), (*Wakeup)(nil), (*Trigger)(nil)} {
		if _, err := tx.NewDelete().Model(model).
			Where("session_id = ?", id).
			Exec(ctx); err != nil {
			return fmt.Errorf("deleting session %s data: %w", id, err)
		}
	}
	if _, err := tx.NewDelete().Model((*Task)(nil)).
		Where("parent_session_id = ?", id).
		WhereOr("child_session_id = ?", id).
		Exec(ctx); err != nil {
		return fmt.Errorf("deleting tasks of session %s: %w", id, err)
	}
	res, err := tx.NewDelete().Model((*Session)(nil)).Where("id = ?", id).Exec(ctx)
	if err == nil && mustExist {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("deleting session %s: %w", id, err)
	}
	return nil
}
