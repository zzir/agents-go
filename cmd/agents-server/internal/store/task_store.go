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

// liveParent and liveChild scope a task row to the session GENERATION that
// answers to its session id right now. A row whose session was deleted — and
// whose id may since belong to a different session — matches neither, so it
// lists nowhere, owes no wake-up and resolves no run. COALESCE covers a
// session row that is gone entirely, which reads as the empty generation and
// therefore matches nothing a live session bound.
const (
	liveParent = `t.parent_session_gen = COALESCE(` +
		`(SELECT s.gen FROM sessions AS s WHERE s.id = t.parent_session_id), '')`
	liveChild = `t.child_session_gen = COALESCE(` +
		`(SELECT s.gen FROM sessions AS s WHERE s.id = t.child_session_id), '')`
	// genOf reads the generation currently answering to a session id, for
	// binding a row at insert.
	genOf = `COALESCE((SELECT s.gen FROM sessions AS s WHERE s.id = ?), '')`
)

// Create inserts the task row (status should be protocol.TaskWorking).
func (s *TaskStore) Create(ctx context.Context, t *Task) error {
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	// The generations are read and the row written in ONE statement: resolving
	// them first and inserting after leaves a window where the session is
	// deleted in between, and the row would bind to a generation that is
	// already gone while claiming to be current.
	if _, err := s.db.NewInsert().Model(t).
		Value("parent_session_gen", genOf, t.ParentSessionID).
		Value("child_session_gen", genOf, t.ChildSessionID).
		Exec(ctx); err != nil {
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
		Where("parent_session_id = ?", parentSessionID).Where(liveParent).
		OrderExpr("created_at DESC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing tasks for session %s: %w", parentSessionID, err)
	}
	return tasks, nil
}

// Task status values, mirrored from protocol (store cannot import bridge's
// protocol package without a cycle; the vocabulary is fixed by MCP Tasks).
const (
	taskWorking       = "working"
	taskInputRequired = "input_required"
)

// taskTerminalSet is the SQL fragment matching terminal statuses.
const taskTerminalSet = "('completed', 'failed', 'cancelled')"

// NotifyState values for the completion wake-up owed to the parent session.
const (
	NotifyPending   = "pending"
	NotifyConsumed  = "consumed"
	NotifyDelivered = "delivered"
)

// Finalize records a terminal status via compare-and-set: it wins only while
// the row is still non-terminal, so of two racing finalizers (stop vs. run
// completion vs. reaper) exactly one lands and a terminal state is never
// overwritten. The same UPDATE owes the parent its wake-up notification
// (notify_state = pending) — result persistence and the notification debt are
// one atomic transition, which is what lets task_status treat "row terminal"
// as "result is fully readable".
func (s *TaskStore) Finalize(ctx context.Context, id, status, summary, result string) (bool, error) {
	q := s.db.NewUpdate().Model((*Task)(nil)).
		Set("status = ?", status).
		Set("notify_state = ?", NotifyPending).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status NOT IN " + taskTerminalSet)
	if summary != "" {
		q = q.Set("summary = ?", summary)
	}
	if result != "" {
		q = q.Set("result = ?", result)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("finalizing task %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkInputRequired flips a working task to input_required (the run paused on
// an approval). Best-effort CAS: a concurrent terminal transition wins.
func (s *TaskStore) MarkInputRequired(ctx context.Context, id string) error {
	if _, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("status = ?", taskInputRequired).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status = ?", taskWorking).
		Exec(ctx); err != nil {
		return fmt.Errorf("marking task %s input_required: %w", id, err)
	}
	return nil
}

// ReclaimWorking flips an input_required task back to working — the approve
// path's exclusive claim against a concurrent stop. Reports whether this call
// won; a false return means the task reached a terminal state meanwhile (it
// was stopped or reaped) and the resume must be abandoned.
func (s *TaskStore) ReclaimWorking(ctx context.Context, id string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("status = ?", taskWorking).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status = ?", taskInputRequired).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("reclaiming task %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ConsumeNotify marks a pending wake-up as consumed: the model already pulled
// the final result in-turn (task_status), so no wake-up run is owed. A no-op
// once the notification was delivered (a later status poll is an idempotent
// read, not a second consumption). Notification bookkeeping deliberately does
// NOT touch updated_at: for a terminal task that column is its finish time
// (created_at → updated_at is the duration the UI shows), and delivery can
// happen much later.
func (s *TaskStore) ConsumeNotify(ctx context.Context, id string) error {
	if _, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("notify_state = ?", NotifyConsumed).
		Where("id = ?", id).
		Where("notify_state = ?", NotifyPending).
		Exec(ctx); err != nil {
		return fmt.Errorf("consuming task %s notification: %w", id, err)
	}
	return nil
}

// MarkNotifyDelivered records that the wake-up run carrying this task's
// result was injected into the parent session. Like ConsumeNotify it leaves
// updated_at alone — see there.
func (s *TaskStore) MarkNotifyDelivered(ctx context.Context, id string) error {
	if _, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("notify_state = ?", NotifyDelivered).
		Where("id = ?", id).
		Where("notify_state = ?", NotifyPending).
		Exec(ctx); err != nil {
		return fmt.Errorf("marking task %s notification delivered: %w", id, err)
	}
	return nil
}

// ListPendingNotify returns the parent session's tasks that still owe it a
// completion wake-up, oldest first (the wake-up message lists them in order).
func (s *TaskStore) ListPendingNotify(ctx context.Context, parentSessionID string) ([]Task, error) {
	var tasks []Task
	if err := s.db.NewSelect().Model(&tasks).
		Where("parent_session_id = ?", parentSessionID).Where(liveParent).
		Where("notify_state = ?", NotifyPending).
		OrderExpr("updated_at ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing pending notifications for %s: %w", parentSessionID, err)
	}
	return tasks, nil
}

// PendingNotifyParents returns every parent session owed at least one wake-up
// — the startup reconciliation sweep that makes the auto-wake survive
// restarts.
func (s *TaskStore) PendingNotifyParents(ctx context.Context) ([]string, error) {
	var ids []string
	if err := s.db.NewSelect().Model((*Task)(nil)).
		ColumnExpr("DISTINCT parent_session_id").
		Where("notify_state = ?", NotifyPending).
		Where(liveParent).
		Scan(ctx, &ids); err != nil {
		return nil, fmt.Errorf("listing notify-pending parents: %w", err)
	}
	return ids, nil
}

// DeleteByID removes a task row — only used to unwind a spawn whose run never
// started (the tool error is the model's record of that attempt).
func (s *TaskStore) DeleteByID(ctx context.Context, id string) error {
	if _, err := s.db.NewDelete().Model((*Task)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("deleting task %s: %w", id, err)
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
		// The failure is news the parent session never heard — owe it the
		// wake-up so the startup drain can deliver it.
		Set("notify_state = ?", NotifyPending).
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
	if err := s.db.NewSelect().Model(t).
		Where("child_session_id = ?", childSessionID).Where(liveChild).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting task for session %s: %w", childSessionID, err)
	}
	return t, nil
}
