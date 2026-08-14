package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/tasks"
)

// taskRow is the persisted form of a tasks.Task.
type taskRow struct {
	bun.BaseModel `bun:"table:agent_tasks,alias:t"`

	ID    string `bun:"id,pk"`
	RunID string `bun:"run_id,notnull"`
	Label string `bun:"label"`

	ParentSessionID string `bun:"parent_session_id,notnull"`
	// ParentSessionGen and ChildSessionGen are the GENERATIONS of the sessions
	// this row names (session.Ref). A session id names a session, not a
	// place: deleting an id and creating it again yields a different session,
	// and a task row that matched on the id alone attached itself to the
	// replacement — listing a dead incarnation's tasks under the new one and
	// owing its wake-ups to a conversation that never spawned them.
	//
	// The store fills them from agent_sessions at insert and every read
	// compares them against the generation that answers to the id NOW (see
	// liveParent / liveChild), so a stale row is inert rather than wrong. The
	// tasks API keeps addressing sessions by id — that is the host's
	// vocabulary — and this column is what makes an id-keyed query safe.
	ParentSessionGen string `bun:"parent_session_gen"`
	ParentRunID      string `bun:"parent_run_id"`
	ToolCallID       string `bun:"tool_call_id"`
	ChildSessionID   string `bun:"child_session_id,notnull"`
	ChildSessionGen  string `bun:"child_session_gen"`
	Depth            int    `bun:"depth,notnull"`
	// Attempt counts this task's runs. Zero reads as the first attempt, which
	// is what a row written before retries existed had.
	Attempt int `bun:"attempt"`

	Inherit string `bun:"inherit"`

	Status  string `bun:"status,notnull"`
	Summary string `bun:"summary"`
	Result  string `bun:"result"`

	CreatedAt time.Time `bun:"created_at,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}

func (r *taskRow) toTask() tasks.Task {
	t := tasks.Task{
		ID:              r.ID,
		RunID:           r.RunID,
		Label:           r.Label,
		ParentSessionID: r.ParentSessionID,
		ParentRunID:     r.ParentRunID,
		ToolCallID:      r.ToolCallID,
		ChildSessionID:  r.ChildSessionID,
		Depth:           r.Depth,
		Attempt:         r.Attempt,
		Status:          tasks.Status(r.Status),
		Summary:         r.Summary,
		Result:          r.Result,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
	if r.Inherit != "" {
		t.Inherit = []byte(r.Inherit)
	}
	return t
}

func rowFrom(t *tasks.Task) *taskRow {
	return &taskRow{
		ID:              t.ID,
		RunID:           t.RunID,
		Label:           t.Label,
		ParentSessionID: t.ParentSessionID,
		ParentRunID:     t.ParentRunID,
		ToolCallID:      t.ToolCallID,
		ChildSessionID:  t.ChildSessionID,
		Depth:           t.Depth,
		Attempt:         t.Attempt,
		Inherit:         string(t.Inherit),
		Status:          string(t.Status),
		Summary:         t.Summary,
		Result:          t.Result,
		CreatedAt:       t.CreatedAt,
		UpdatedAt:       t.UpdatedAt,
	}
}

// TaskStore is a SQL-backed tasks.Store.
//
// The SQL is what makes it usable: Finalize is one conditional UPDATE, so the
// database arbitrates between racing finalizers rather than the process. That
// is why tasks require a transactional store and why there is no file-backed
// one — a read-modify-write over JSON cannot offer the same guarantee across
// processes.
type TaskStore struct {
	db *bun.DB
}

// NewTaskStore wraps a *bun.DB as a task store. Call CreateTaskSchema once
// before first use.
func NewTaskStore(db *bun.DB) *TaskStore { return &TaskStore{db: db} }

// CreateTaskSchema creates the task table and its indexes.
//
// It also ensures the session table exists, because a task row names sessions
// by (id, generation) and every read resolves the generation against it — a
// task store built without it would fail at the first query rather than at
// setup, depending on the order the two schema calls happened to be made in.
// Both creations are IfNotExists, so calling this and CreateSchema in either
// order (or both) is the same.
func CreateTaskSchema(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*sessionRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	if _, err := db.NewCreateTable().Model((*taskRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	// Listing a parent's tasks and finding a task by its child session are the
	// two lookups on every run boundary; without these they are table scans.
	for name, cols := range map[string][]string{
		"idx_agent_tasks_parent": {"parent_session_id", "parent_session_gen"},
		"idx_agent_tasks_child":  {"child_session_id", "child_session_gen"},
	} {
		if _, err := db.NewCreateIndex().Model((*taskRow)(nil)).
			Index(name).Column(cols...).IfNotExists().Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// terminalStatuses is the statuses this store treats as final, for the two
// queries that have to filter on them in SQL.
//
// tasks.Status.Terminal is the source of truth for what "final" means; a
// method cannot run inside a WHERE clause, so the set is mirrored here — from
// the tasks constants rather than as bare strings. A test drives a task into
// each status it knows and checks the mirror against Terminal(), which catches
// one changing sides but not a status ADDED upstream: tasks exports no
// enumeration to walk, so a new one has to be added here and there by hand.
var terminalStatuses = []string{
	string(tasks.StatusCompleted),
	string(tasks.StatusFailed),
	string(tasks.StatusCancelled),
}

// liveParent and liveChild scope a task row to the session GENERATION that
// answers to its session id right now. A row whose session was deleted — and
// whose id may since belong to a different session — matches neither, so it
// lists nowhere, owes no wake-up and resolves no run.
//
// COALESCE covers a session with no row at all: sessions.New(db, id) is the
// direct scope, which has no agent_sessions entry, and its tasks store the
// empty generation for the same reason its entries do.
const (
	liveParent = `t.parent_session_gen = COALESCE(` +
		`(SELECT s.gen FROM agent_sessions AS s WHERE s.id = t.parent_session_id), '')`
	liveChild = `t.child_session_gen = COALESCE(` +
		`(SELECT s.gen FROM agent_sessions AS s WHERE s.id = t.child_session_id), '')`
	// genOf reads the generation currently answering to a session id, for
	// binding a row at insert. Same shape as above, as a value expression.
	genOf = `COALESCE((SELECT s.gen FROM agent_sessions AS s WHERE s.id = ?), '')`
)

// Create implements tasks.Store.
func (s *TaskStore) Create(ctx context.Context, t *tasks.Task) error {
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	// The generations are read and the row written in ONE statement: resolving
	// them first and inserting after leaves a window where the session is
	// deleted in between, and the row would bind to a generation that no
	// longer exists while claiming to be current.
	if _, err := s.db.NewInsert().Model(rowFrom(t)).
		Value("parent_session_gen", genOf, t.ParentSessionID).
		Value("child_session_gen", genOf, t.ChildSessionID).
		Exec(ctx); err != nil {
		return fmt.Errorf("creating task %q: %w", t.ID, err)
	}
	return nil
}

// Get implements tasks.Store.
func (s *TaskStore) Get(ctx context.Context, id string) (*tasks.Task, error) {
	row := new(taskRow)
	if err := s.db.NewSelect().Model(row).Where("t.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, tasks.ErrNotFound
		}
		return nil, fmt.Errorf("getting task %q: %w", id, err)
	}
	t := row.toTask()
	return &t, nil
}

// ByChildSession implements tasks.Store.
func (s *TaskStore) ByChildSession(ctx context.Context, sessionID string) (*tasks.Task, error) {
	row := new(taskRow)
	if err := s.db.NewSelect().Model(row).
		Where("child_session_id = ?", sessionID).Where(liveChild).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, tasks.ErrNotFound
		}
		return nil, fmt.Errorf("getting task for session %q: %w", sessionID, err)
	}
	t := row.toTask()
	return &t, nil
}

// ListByParent implements tasks.Store, newest first.
func (s *TaskStore) ListByParent(ctx context.Context, parentSessionID string) ([]tasks.Task, error) {
	return s.query(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("parent_session_id = ?", parentSessionID).Where(liveParent).
			OrderExpr("created_at DESC")
	})
}

// ListNonTerminal implements tasks.Store.
func (s *TaskStore) ListNonTerminal(ctx context.Context, parentSessionID string) ([]tasks.Task, error) {
	return s.query(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("parent_session_id = ?", parentSessionID).Where(liveParent).
			Where("status NOT IN (?)", bun.List(terminalStatuses))
	})
}

func (s *TaskStore) query(ctx context.Context, apply func(*bun.SelectQuery) *bun.SelectQuery) ([]tasks.Task, error) {
	var rows []taskRow
	if err := apply(s.db.NewSelect().Model(&rows)).Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	out := make([]tasks.Task, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toTask())
	}
	return out, nil
}

// Finalize implements tasks.Store as one conditional UPDATE.
//
// Status and result land together, and only while the row is still
// non-terminal. Writing them separately would let a reader see a terminal
// task whose result has not arrived — which is exactly what task_status must
// be able to rely on.
// The run_id predicate is the other half: a task can leave a terminal state
// now (RetryClaim), so non-terminality alone no longer says WHICH attempt the
// finalizer looked at.
func (s *TaskStore) Finalize(ctx context.Context, id, runID string, st tasks.Status, summary, result string) (bool, error) {
	q := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(st)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status NOT IN (?)", bun.List(terminalStatuses))
	if summary != "" {
		q = q.Set("summary = ?", summary)
	}
	if result != "" {
		q = q.Set("result = ?", result)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("finalizing task %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RetryClaim implements tasks.Store as one conditional UPDATE, so the attempt
// ceiling holds across processes rather than only within the Manager that
// checked it.
//
// The generation predicate is the same one every by-session read carries: a row
// whose sessions were deleted must not come back to life and launch a run onto
// an id that now answers to a different session.
func (s *TaskStore) RetryClaim(ctx context.Context, id, newRunID string, maxAttempts int) (bool, error) {
	q := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusWorking)).
		Set("run_id = ?", newRunID).
		// Zero counts as the first attempt, here as everywhere.
		Set("attempt = CASE WHEN attempt < 1 THEN 2 ELSE attempt + 1 END").
		Set("summary = ?", "").
		Set("result = ?", "").
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status = ?", string(tasks.StatusFailed)).
		Where(liveParent).Where(liveChild)
	if maxAttempts > 0 {
		q = q.Where("CASE WHEN attempt < 1 THEN 1 ELSE attempt END < ?", maxAttempts)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("claiming a retry of task %q: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return true, nil
	}
	// Zero rows is "could not claim" or "no such task", and the two mean
	// different things to a caller — the same disambiguation ReclaimWorking
	// makes, for the same reason: two shipped Stores must not answer one call
	// differently.
	exists, eerr := s.db.NewSelect().Model((*taskRow)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return false, fmt.Errorf("claiming a retry of task %q: %w", id, eerr)
	}
	if !exists {
		return false, tasks.ErrNotFound
	}
	return false, nil
}

// ReleaseRetryClaim implements tasks.Store as one conditional UPDATE bound to
// the claimed run id, like Finalize: only the claim's owner can release it.
// The attempt rolls back because the claimed run never launched — attempt
// counts runs the task has HAD — with the same floor AttemptNo() applies.
func (s *TaskStore) ReleaseRetryClaim(ctx context.Context, id, runID, summary, result string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusFailed)).
		Set("attempt = CASE WHEN attempt <= 1 THEN 1 ELSE attempt - 1 END").
		Set("summary = ?", summary).
		Set("result = ?", result).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status = ?", string(tasks.StatusWorking)).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("releasing the retry claim of task %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkInputRequired implements tasks.Store, bound to the current attempt like
// Finalize — an approval can outlive the attempt that opened it, and it must
// not pause the one that replaced it. Best-effort CAS: a concurrent terminal
// transition (or newer attempt) wins.
func (s *TaskStore) MarkInputRequired(ctx context.Context, id, runID string) error {
	_, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusInputRequired)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status = ?", string(tasks.StatusWorking)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("marking task %q input_required: %w", id, err)
	}
	return nil
}

// ReclaimWorking implements tasks.Store, with the same attempt bound: a stale
// approval (its attempt retried past) must lose the claim, not resume over
// the newer run.
func (s *TaskStore) ReclaimWorking(ctx context.Context, id, runID string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusWorking)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status = ?", string(tasks.StatusInputRequired)).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("reclaiming task %q: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return true, nil
	}
	// Zero rows is either "not in input_required" (lost the claim) or "no such
	// task", and the two mean different things to a caller. The in-memory
	// store distinguishes them, so this one must too — two shipped Stores
	// answering one call differently is how a caller ends up correct against
	// only the backend it was written against.
	exists, eerr := s.db.NewSelect().Model((*taskRow)(nil)).Where("id = ?", id).Exists(ctx)
	if eerr != nil {
		return false, fmt.Errorf("reclaiming task %q: %w", id, eerr)
	}
	if !exists {
		return false, tasks.ErrNotFound
	}
	return false, nil
}

// FailOrphans implements tasks.Store, as ONE statement so the rows reported
// are exactly the rows failed — a select-then-update pair could fail a row the
// select never saw, or report one that finalized in between. Scoped by
// liveParent: a row bound to a dead generation matches nothing (§2.13 — it
// lists nowhere and owes nothing), so the sweep neither fails nor reports it.
//
// input_required rows are kept: their pending approval persists and resumes
// the run, so they are not orphans.
func (s *TaskStore) FailOrphans(ctx context.Context) ([]tasks.Task, error) {
	const summary = "the process restarted while the task was running"
	var rows []taskRow
	if _, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusFailed)).
		Set("summary = ?", summary).
		Set("updated_at = ?", time.Now().UTC()).
		Where("status = ?", string(tasks.StatusWorking)).
		Where(liveParent).
		Returning("*").
		Exec(ctx, &rows); err != nil {
		return nil, fmt.Errorf("failing orphaned tasks: %w", err)
	}
	out := make([]tasks.Task, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toTask())
	}
	return out, nil
}

// Delete implements tasks.Store.
func (s *TaskStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.NewDelete().Model((*taskRow)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("deleting task %q: %w", id, err)
	}
	return nil
}

var _ tasks.Store = (*TaskStore)(nil)
