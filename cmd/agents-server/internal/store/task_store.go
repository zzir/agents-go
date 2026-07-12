package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// TaskStore persists background task rows. The row is the durable truth for a
// task's identity and terminal outcome — the hub's RunInfo is memory-only and
// GC'd minutes after the run ends, so reloads rebuild task state from here.
type TaskStore struct {
	db *bun.DB
}

// NewTaskStore returns a store backed by db.
func NewTaskStore(db *bun.DB) *TaskStore { return &TaskStore{db: db} }

// Create inserts the task row (status should be protocol.TaskWorking).
func (s *TaskStore) Create(ctx context.Context, t *Task) error {
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if _, err := s.db.NewInsert().Model(t).Exec(ctx); err != nil {
		return fmt.Errorf("creating task: %w", err)
	}
	return nil
}

// Get returns the task with the given id (== its run id).
func (s *TaskStore) Get(ctx context.Context, id string) (*Task, error) {
	t := new(Task)
	if err := s.db.NewSelect().Model(t).Where("t.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting task %s: %w", id, err)
	}
	return t, nil
}

// ListByParent returns the tasks spawned from the given chat session, newest
// first.
func (s *TaskStore) ListByParent(ctx context.Context, parentSessionID string) ([]Task, error) {
	var tasks []Task
	if err := s.db.NewSelect().Model(&tasks).
		Where("parent_session_id = ?", parentSessionID).
		OrderExpr("created_at DESC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing tasks for session %s: %w", parentSessionID, err)
	}
	return tasks, nil
}

// SetStatus records a task's (possibly terminal) status, its truncated
// summary, and — when the run produced one — the full final output.
func (s *TaskStore) SetStatus(ctx context.Context, id, status, summary, result string) error {
	q := s.db.NewUpdate().Model((*Task)(nil)).
		Set("status = ?", status).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id)
	if summary != "" {
		q = q.Set("summary = ?", summary)
	}
	if result != "" {
		q = q.Set("result = ?", result)
	}
	if _, err := q.Exec(ctx); err != nil {
		return fmt.Errorf("updating task %s: %w", id, err)
	}
	return nil
}

// ChildSessionIDs returns the hidden child-session ids of every task, used to
// filter task transcripts out of the chat session list.
func (s *TaskStore) ChildSessionIDs(ctx context.Context) ([]string, error) {
	var ids []string
	if err := s.db.NewSelect().Model((*Task)(nil)).
		Column("child_session_id").
		Scan(ctx, &ids); err != nil {
		return nil, fmt.Errorf("listing task sessions: %w", err)
	}
	return ids, nil
}

// FailOrphans marks every task still recorded as working as failed. Called
// once at startup: task runs do not survive a process restart, and their
// terminal status is written by the run goroutine — a row stuck at "working"
// after boot can never progress. input_required rows are kept: their pending
// approval persists and resumes the run.
func (s *TaskStore) FailOrphans(ctx context.Context) (int64, error) {
	res, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("status = ?", "failed").
		Set("summary = ?", "server restarted while the task was running").
		Set("updated_at = ?", time.Now().UTC()).
		Where("status = ?", "working").
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("failing orphaned tasks: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ByChildSession returns the task owning the given hidden child session, or
// ErrNotFound.
func (s *TaskStore) ByChildSession(ctx context.Context, childSessionID string) (*Task, error) {
	t := new(Task)
	if err := s.db.NewSelect().Model(t).Where("child_session_id = ?", childSessionID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting task for session %s: %w", childSessionID, err)
	}
	return t, nil
}
