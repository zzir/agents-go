package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// TaskStore persists background task rows — the durable truth for a task's
// identity and terminal outcome (the hub's RunInfo is memory-only).
type TaskStore struct {
	db *bun.DB
}

// NewTaskStore returns a store backed by db.
func NewTaskStore(db *bun.DB) *TaskStore { return &TaskStore{db: db} }

// liveParent and liveChild scope a task row to the session GENERATION that
// answers to its session id right now (invariant 23); COALESCE makes a gone session match nothing.
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
	// The generations are read and the row written in ONE statement, so the
	// row cannot bind to a generation deleted in between.
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

// TaskWithSession is a task row with the name of the conversation it belongs
// to, for a listing that spans sessions.
type TaskWithSession struct {
	Task        `bun:",extend"`
	SessionName string `bun:"session_name,scanonly" json:"session_name"`
}

// ListRecent returns one page of tasks across every live session, newest
// first (of one kind when kind is set, still-live only when liveOnly, one
// owner's when ownerID is set), and the total. limit is capped at 500 (0 = the cap).
func (s *TaskStore) ListRecent(ctx context.Context, ownerID, kind string, liveOnly bool, limit, offset int) (rows []TaskWithSession, total int, err error) {
	if ownerID == "" {
		return nil, 0, errListNoOwner
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	filter := func(q *bun.SelectQuery) *bun.SelectQuery {
		// A hidden parent is a task's own session: its tasks are nested work,
		// with no conversation of their own to open.
		q = q.Join("JOIN sessions AS ps ON ps.id = t.parent_session_id").Where(liveParent).Where("ps.hidden = ?", false)
		if ownerID != EveryOwner {
			q = q.Where("ps.owner_id = ?", ownerID)
		}
		if kind != "" {
			q = q.Where("t.kind = ?", kind)
		}
		if liveOnly {
			q = q.Where("t.status NOT IN " + taskTerminalSet)
		}
		return q
	}
	total, err = filter(s.db.NewSelect().Model((*Task)(nil))).Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("counting recent tasks: %w", err)
	}
	if err := filter(s.db.NewSelect().Model((*Task)(nil))).
		ColumnExpr("t.*").
		ColumnExpr("ps.name AS session_name").
		OrderExpr("t.created_at DESC, t.id DESC").
		Limit(limit).Offset(offset).
		Scan(ctx, &rows); err != nil {
		return nil, 0, fmt.Errorf("listing recent tasks: %w", err)
	}
	return rows, total, nil
}

// ListNonTerminalByParent returns the given chat session's still-live tasks,
// liveParent like every other by-session read (invariant 23).
func (s *TaskStore) ListNonTerminalByParent(ctx context.Context, parentSessionID string) ([]Task, error) {
	var tasks []Task
	if err := s.db.NewSelect().Model(&tasks).
		Where("parent_session_id = ?", parentSessionID).Where(liveParent).
		Where("status NOT IN " + taskTerminalSet).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing live tasks for %s: %w", parentSessionID, err)
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

// Finalize is the CAS to a terminal status (on non-terminality AND the
// attempt named by runID) plus the wake-up debt, in one transaction —
// invariant 32. buildWakeup reads the row in the SAME tx; nil owes nothing,
// and the debt is written only when the CAS won.
func (s *TaskStore) Finalize(ctx context.Context, id, runID, status, summary, result string, state json.RawMessage, buildWakeup func(*Task) *Wakeup) (bool, error) {
	var won bool
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		row, err := taskRowForWakeup(ctx, tx, id, buildWakeup)
		if err != nil {
			return err
		}
		q := tx.NewUpdate().Model((*Task)(nil)).
			Set("status = ?", status).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Where("run_id = ?", runID).
			Where("status NOT IN " + taskTerminalSet)
		if summary != "" {
			q = q.Set("summary = ?", summary)
		}
		if result != "" {
			q = q.Set("result = ?", result)
		}
		if state != nil {
			// The job's final state, in the transition itself — the wake-up the
			// row owes is written from the row as it stands here too.
			q = q.Set("state = ?", string(state))
			if row != nil {
				row.State = state
			}
		}
		res, err := q.Exec(ctx)
		if err != nil {
			return fmt.Errorf("finalizing task %s: %w", id, err)
		}
		n, _ := res.RowsAffected()
		won = n > 0
		if won && row != nil {
			if wk := buildWakeup(row); wk != nil {
				if _, err := tx.NewInsert().Model(wk).Exec(ctx); err != nil {
					return fmt.Errorf("recording task %s wake-up: %w", id, err)
				}
			}
		}
		return nil
	})
	return won, err
}

// taskRowForWakeup reads the row a debt will be addressed from, inside the
// caller's tx. A missing row is nil (the CAS reports the miss); any other failure is an error.
func taskRowForWakeup(ctx context.Context, tx bun.Tx, id string, buildWakeup func(*Task) *Wakeup) (*Task, error) {
	if buildWakeup == nil {
		return nil, nil
	}
	row := new(Task)
	if err := tx.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading task %s for its wake-up: %w", id, err)
	}
	return row, nil
}

// RetryClaim implements the tasks.Store contract as one conditional UPDATE,
// so the attempt ceiling holds across processes; generation-fenced (invariant 23).
func (s *TaskStore) RetryClaim(ctx context.Context, id, newRunID string, maxAttempts int) (bool, error) {
	var won bool
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		q := tx.NewUpdate().Model((*Task)(nil)).
			Set("status = ?", taskWorking).
			Set("run_id = ?", newRunID).
			// Zero counts as the first attempt, here as everywhere.
			Set("attempt = CASE WHEN attempt < 1 THEN 2 ELSE attempt + 1 END").
			Set("summary = ?", "").
			Set("result = ?", "").
			// Live again, so back on the strip whatever the person hid.
			Set("dismissed = ?", false).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Where("status = ?", "failed").
			Where(liveParent).Where(liveChild)
		if maxAttempts > 0 {
			q = q.Where("CASE WHEN attempt < 1 THEN 1 ELSE attempt END < ?", maxAttempts)
		}
		res, err := q.Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		won = n > 0
		if won {
			// The prior attempt's failure debt is stale the instant a retry is
			// claimed; cancel it in the SAME tx.
			if _, err := tx.NewUpdate().Model((*Wakeup)(nil)).
				Set("state = ?", WakeCancelled).
				Where("kind = ?", WakeKindTask).
				Where("source_id = ?", id).
				Where("state = ?", WakePending).
				Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("claiming a retry of task %s: %w", id, err)
	}
	if won {
		return true, nil
	}
	// Zero rows is "could not claim" or "no such task"; the SDK's in-memory
	// store distinguishes them, so this one must too.
	exists, eerr := s.db.NewSelect().Model((*Task)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return false, fmt.Errorf("claiming a retry of task %s: %w", id, eerr)
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}

// Advance implements the tasks.Store contract as one conditional UPDATE: the
// run moves and the state lands together, only while runID is current and the row is working.
func (s *TaskStore) Advance(ctx context.Context, id, runID, nextRunID string, state json.RawMessage) (bool, error) {
	q := s.db.NewUpdate().Model((*Task)(nil)).
		Set("run_id = ?", nextRunID).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status = ?", taskWorking).
		Where(liveParent).Where(liveChild)
	if state != nil {
		q = q.Set("state = ?", string(state))
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("advancing task %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return true, nil
	}
	exists, eerr := s.db.NewSelect().Model((*Task)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return false, fmt.Errorf("advancing task %s: %w", id, eerr)
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}

// Dismiss hides a terminal task from the live strip. Terminal-only: a running
// task is exactly what the strip exists to show. Reports whether a row moved.
func (s *TaskStore) Dismiss(ctx context.Context, id string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("dismissed = ?", true).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status IN " + taskTerminalSet).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("dismissing task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("dismissing task %s: %w", id, err)
	}
	return n > 0, nil
}

// ReleaseRetryClaim undoes a RetryClaim whose run never launched: status
// back to failed, the attempt count back down, the launch failure recorded,
// and in the SAME tx a FRESH failure debt (buildWakeup, as in Finalize).
// Bound to the claimed run id: only the claim's owner can release it.
func (s *TaskStore) ReleaseRetryClaim(ctx context.Context, id, runID, summary, result string, buildWakeup func(*Task) *Wakeup) (bool, error) {
	var won bool
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		row, err := taskRowForWakeup(ctx, tx, id, buildWakeup)
		if err != nil {
			return err
		}
		res, err := tx.NewUpdate().Model((*Task)(nil)).
			Set("status = ?", "failed").
			// The floor mirrors AttemptNo(): never below the original run.
			Set("attempt = CASE WHEN attempt <= 1 THEN 1 ELSE attempt - 1 END").
			Set("summary = ?", summary).
			Set("result = ?", result).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Where("run_id = ?", runID).
			Where("status = ?", taskWorking).
			Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		won = n > 0
		if won && row != nil {
			if wk := buildWakeup(row); wk != nil {
				if _, err := tx.NewInsert().Model(wk).Exec(ctx); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("releasing the retry claim of task %s: %w", id, err)
	}
	return won, nil
}

// MarkInputRequired flips a working task to input_required, only while runID
// is the current attempt (an approval can outlive its attempt). Best-effort
// CAS: a concurrent terminal transition or newer attempt wins.
func (s *TaskStore) MarkInputRequired(ctx context.Context, id, runID string) error {
	if _, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("status = ?", taskInputRequired).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status = ?", taskWorking).
		Exec(ctx); err != nil {
		return fmt.Errorf("marking task %s input_required: %w", id, err)
	}
	return nil
}

// Pause holds a working task on a decision, in one transaction: status to
// input_required, the approval filed, the state written when given, all
// under runID — the one write for every pause (invariant 37). Reports
// whether the row was claimed; false means nothing was written.
func (s *TaskStore) Pause(ctx context.Context, id, runID string, state json.RawMessage, approval *PendingApproval) (bool, error) {
	var won bool
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		q := tx.NewUpdate().Model((*Task)(nil)).
			Set("status = ?", taskInputRequired).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Where("run_id = ?", runID).
			Where("status = ?", taskWorking)
		if state != nil {
			q = q.Set("state = ?", string(state))
		}
		res, err := q.Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		won = true
		if approval.CreatedAt.IsZero() {
			approval.CreatedAt = time.Now().UTC()
		}
		_, err = tx.NewInsert().Model(approval).
			On("CONFLICT (run_id) DO UPDATE").
			Set("state = EXCLUDED.state").
			Set("tool_calls = EXCLUDED.tool_calls").
			Exec(ctx)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("pausing task %s: %w", id, err)
	}
	return won, nil
}

// ClaimOutcome is what a decision on a task's approval found: won, already
// claimed by another decision, or the task not paused on that run.
type ClaimOutcome int

// The three answers of a claim.
const (
	ClaimWon ClaimOutcome = iota
	// ClaimTaken: the approval row was already gone — a racing decision won.
	ClaimTaken
	// ClaimTaskNotPaused: the row was there, but the task is not
	// input_required on runID. Nothing was written; the row stays.
	ClaimTaskNotPaused
)

// ClaimApprovalWorking is a decision on a paused task's approval, in one
// transaction: the approval row deleted (the exclusive claim) and the task
// flipped input_required → working under runID — invariant 37. What the
// caller does next is its own launch.
func (s *TaskStore) ClaimApprovalWorking(ctx context.Context, taskID, runID string) (ClaimOutcome, error) {
	outcome := ClaimTaken
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// The row first (its absence means another decision took it), then
		// the task; a task not paused on this run rolls the delete back.
		del, err := tx.NewDelete().Model((*PendingApproval)(nil)).Where("run_id = ?", runID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := del.RowsAffected(); err != nil || n == 0 {
			if err == nil {
				return errRollback
			}
			return err
		}
		res, err := tx.NewUpdate().Model((*Task)(nil)).
			Set("status = ?", taskWorking).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", taskID).
			Where("run_id = ?", runID).
			Where("status = ?", taskInputRequired).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err == nil {
				outcome = ClaimTaskNotPaused
				return errRollback
			}
			return err
		}
		outcome = ClaimWon
		return nil
	})
	if err != nil && !errors.Is(err, errRollback) {
		return ClaimTaken, fmt.Errorf("claiming the approval of task %s: %w", taskID, err)
	}
	return outcome, nil
}

// ClaimApprovalCancelled ends a paused task on its approval, in one
// transaction: the approval row deleted (the claim) and the task finalized
// cancelled (invariant 37). claimed reports whether this call took the row;
// ended whether it moved the task. A cancellation owes no wake-up.
func (s *TaskStore) ClaimApprovalCancelled(ctx context.Context, taskID, runID, summary string) (claimed, ended bool, err error) {
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		del, err := tx.NewDelete().Model((*PendingApproval)(nil)).Where("run_id = ?", runID).Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := del.RowsAffected(); err != nil || n == 0 {
			return err
		}
		claimed = true
		res, err := tx.NewUpdate().Model((*Task)(nil)).
			Set("status = ?", "cancelled").
			Set("summary = ?", summary).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", taskID).
			Where("run_id = ?", runID).
			Where("status NOT IN " + taskTerminalSet).
			Exec(ctx)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		ended = n > 0
		if ended {
			if _, err := tx.NewUpdate().Model((*Wakeup)(nil)).
				Set("state = ?", WakeCancelled).
				Where("kind = ?", WakeKindTask).Where("source_id = ?", taskID).Where("attempt = ?", runID).
				Where("state = ?", WakePending).
				Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, false, fmt.Errorf("cancelling task %s on its approval: %w", taskID, err)
	}
	return claimed, ended, nil
}

// errRollback aborts a claim's transaction without a fault: nothing to write.
var errRollback = errors.New("nothing to write")

// ReclaimWorking flips an input_required task back to working — the approve
// path's exclusive claim against a concurrent stop — only while runID is the
// current attempt. Reports whether this call won; an absent task is
// ErrNotFound (the conformance suite holds every store to that).
func (s *TaskStore) ReclaimWorking(ctx context.Context, id, runID string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*Task)(nil)).
		Set("status = ?", taskWorking).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status = ?", taskInputRequired).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("reclaiming task %s: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return true, nil
	}
	exists, eerr := s.db.NewSelect().Model((*Task)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return false, fmt.Errorf("reclaiming task %s: %w", id, eerr)
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}

// DeleteByID removes a task row — only used to unwind a spawn whose run never
// started (the tool error is the model's record of that attempt).
func (s *TaskStore) DeleteByID(ctx context.Context, id string) error {
	if _, err := s.db.NewDelete().Model((*Task)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("deleting task %s: %w", id, err)
	}
	return nil
}

// FailOrphans fails every task left at "working" by a restart and, in the
// SAME transaction, records the wake-up each owes its parent (buildWakeup;
// nil owes nothing) — invariant 32. input_required rows are kept: their
// pending approval persists and resumes the run.
func (s *TaskStore) FailOrphans(ctx context.Context, buildWakeup func(*Task) *Wakeup) ([]Task, error) {
	var orphans []Task
	const summary = "server restarted while the task was running"
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Read first, then write: the caller has to TELL each parent, and an
		// update that only reports a count leaves it with nobody to tell.
		if err := tx.NewSelect().Model(&orphans).Where("status = ?", "working").Scan(ctx); err != nil {
			return fmt.Errorf("listing orphaned tasks: %w", err)
		}
		if len(orphans) == 0 {
			return nil
		}
		if _, err := tx.NewUpdate().Model((*Task)(nil)).
			Set("status = ?", "failed").
			Set("summary = ?", summary).
			Set("updated_at = ?", time.Now().UTC()).
			Where("status = ?", "working").
			Exec(ctx); err != nil {
			return fmt.Errorf("failing orphaned tasks: %w", err)
		}
		for i := range orphans {
			orphans[i].Status, orphans[i].Summary = "failed", summary
			if buildWakeup == nil {
				continue
			}
			if wk := buildWakeup(&orphans[i]); wk != nil {
				if _, err := tx.NewInsert().Model(wk).Exec(ctx); err != nil {
					return fmt.Errorf("recording orphan %s wake-up: %w", orphans[i].ID, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return orphans, nil
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
