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

// ListNonTerminalByParent returns the given chat session's still-live tasks.
// liveParent like every other by-session read (spec §2.13): without it a dead
// incarnation's rows still counted against the live session's concurrency cap
// — an ErrTaskLimit nothing could clear, since the list the user sees is
// guarded — and StopTree cancelled a previous incarnation's tasks.
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

// NotifyState values for the completion wake-up owed to the parent session.

// Finalize is the CAS to a terminal status (on non-terminality AND the attempt
// named by runID), plus the wake-up the finished task owes its parent — BOTH in
// one transaction, so a crash can never leave a completed task whose parent is
// never told. buildWakeup turns the row — read in the SAME tx, so a failed
// read aborts rather than silently dropping the debt — into what is owed; nil
// (the function, or its answer) owes nothing. The debt is written only when
// the CAS actually won: a superseded attempt owes nothing.
func (s *TaskStore) Finalize(ctx context.Context, id, runID, status, summary, result string, buildWakeup func(*Task) *Wakeup) (bool, error) {
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
// caller's tx. A missing row is nil (the CAS will report the miss); any other
// read failure is an error — proceeding without it would finalize a task and
// silently lose what its parent is owed.
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
// so the attempt ceiling holds when two processes ask at once rather than
// only inside the caller that checked it first. The generation predicates
// are the ones every by-session read carries: a row whose sessions are gone
// must not come back to life and start a run on an id that now answers to
// someone else.
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
			// claimed — otherwise a busy parent would still be told the old
			// failure while the new attempt runs. Cancel it in the SAME tx.
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
	// Zero rows is "could not claim" or "no such task", and a caller acts
	// differently on each — the SDK's in-memory store distinguishes them, so
	// this one must too.
	exists, eerr := s.db.NewSelect().Model((*Task)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return false, fmt.Errorf("claiming a retry of task %s: %w", id, eerr)
	}
	if !exists {
		return false, ErrNotFound
	}
	return false, nil
}

// ReleaseRetryClaim undoes a RetryClaim whose run never launched: status back
// to failed, the attempt count back down (the claimed run started nothing, and
// attempt counts runs the task has HAD), the launch failure recorded, and — in
// the SAME tx — a FRESH failure debt owed (buildWakeup, as in Finalize).
// RetryClaim cancelled the prior debt, so without this a launch that failed
// after an already-delivered failure would leave the parent with no notice at
// all. Bound to the claimed run id like Finalize: only the claim's owner can
// release it.
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

// MarkInputRequired flips a working task to input_required (the run paused on
// an approval), only while runID is the current attempt — an approval can
// outlive its attempt (crash before this mark, FailOrphans, retry), and it
// must not pause the attempt that replaced its own. Best-effort CAS: a
// concurrent terminal transition (or newer attempt) wins.
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

// ReclaimWorking flips an input_required task back to working — the approve
// path's exclusive claim against a concurrent stop — only while runID is the
// current attempt. Reports whether this call won; false means the task went
// terminal, was retried past this attempt (the approval is stale and must be
// discarded, not retried), or is not paused. A task that does not exist is
// ErrNotFound — a different answer, and the shipped stores must agree on it
// (the conformance suite holds all three to that).
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

// FailOrphans fails every task left at "working" by a restart (a task run does
// not survive the process, so such a row can never progress) and, in the SAME
// transaction, records the wake-up each owes its parent — buildWakeup turns a
// failed row into the debt (nil owes nothing). Atomic for the same reason
// Finalize is: an orphan failed but never notified is a parent that waits
// forever. input_required rows are kept: their pending approval persists and
// resumes the run.
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
