package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
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
	Kind  string `bun:"kind"`

	ParentSessionID string `bun:"parent_session_id,notnull"`
	// ParentSessionGen and ChildSessionGen are the GENERATIONS of the sessions this
	// row names (session.Ref), filled at insert and compared on every read
	// (liveParent / liveChild) — spec §2.13.
	ParentSessionGen string `bun:"parent_session_gen"`
	ParentRunID      string `bun:"parent_run_id"`
	ToolCallID       string `bun:"tool_call_id"`
	ChildSessionID   string `bun:"child_session_id,notnull"`
	ChildSessionGen  string `bun:"child_session_gen"`
	Depth            int    `bun:"depth,notnull"`
	// Attempt counts this task's runs; zero reads as the first attempt.
	Attempt int `bun:"attempt"`

	Inherit string `bun:"inherit"`
	// State is the host's opaque JSON, stored as text like Inherit.
	State string `bun:"state"`

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
		Kind:            r.Kind,
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
	if r.State != "" {
		t.State = []byte(r.State)
	}
	return t
}

func rowFrom(t *tasks.Task) *taskRow {
	return &taskRow{
		ID:              t.ID,
		RunID:           t.RunID,
		Label:           t.Label,
		Kind:            t.Kind,
		ParentSessionID: t.ParentSessionID,
		ParentRunID:     t.ParentRunID,
		ToolCallID:      t.ToolCallID,
		ChildSessionID:  t.ChildSessionID,
		Depth:           t.Depth,
		Attempt:         t.Attempt,
		Inherit:         string(t.Inherit),
		State:           string(t.State),
		Status:          string(t.Status),
		Summary:         t.Summary,
		Result:          t.Result,
		CreatedAt:       t.CreatedAt,
		UpdatedAt:       t.UpdatedAt,
	}
}

// TaskStore is a SQL-backed tasks.Store: Finalize is one conditional UPDATE,
// so the database arbitrates between racing finalizers (decisions §5.54).
type TaskStore struct {
	db *bun.DB
}

// NewTaskStore wraps a *bun.DB as a task store. Call CreateTaskSchema once
// before first use.
func NewTaskStore(db *bun.DB) *TaskStore { return &TaskStore{db: db} }

// CreateTaskSchema creates the task table and its indexes, and the session
// table a task row resolves its generations against; every creation is
// IfNotExists, so this and CreateSchema compose in either order.
func CreateTaskSchema(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*sessionRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	if _, err := db.NewCreateTable().Model((*taskRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	// The two lookups on every run boundary: ListByParent and ByChildSession.
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

// terminalStatuses mirrors tasks.Status.Terminal for the WHERE clauses that
// need it; a test checks the mirror, but a status ADDED to tasks must be added here.
var terminalStatuses = []string{
	string(tasks.StatusCompleted),
	string(tasks.StatusFailed),
	string(tasks.StatusCancelled),
}

// liveParent and liveChild scope a task row to the session GENERATION its id
// answers to now (spec §2.13); COALESCE covers a direct-scope session with no row.
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
	// The generations are read and the row written in ONE statement, so a session
	// deleted in between cannot leave a row bound to a gone generation.
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

// Finalize implements tasks.Store as one conditional UPDATE: status and result
// land together, only while the row is non-terminal and runID is the current
// attempt (spec §2.13).
func (s *TaskStore) Finalize(ctx context.Context, id, runID string, st tasks.Status, summary, result string, state json.RawMessage) (bool, error) {
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
	if state != nil {
		q = q.Set("state = ?", string(state))
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("finalizing task %q: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return true, nil
	}
	return false, s.casMiss(ctx, id)
}

// casMiss classifies a CAS that changed no row: an absent id is ErrNotFound,
// a present one a lost claim (nil, for won=false). Every store answers alike.
func (s *TaskStore) casMiss(ctx context.Context, id string) error {
	exists, err := s.db.NewSelect().Model((*taskRow)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return fmt.Errorf("looking up task %q: %w", id, err)
	}
	if !exists {
		return tasks.ErrNotFound
	}
	return nil
}

// RetryClaim implements tasks.Store as one conditional UPDATE, so the attempt
// ceiling holds across processes; the generation predicates keep a row whose
// sessions were deleted from launching a run (spec §2.13).
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
	return false, s.casMiss(ctx, id)
}

// Advance implements tasks.Store as one conditional UPDATE: the run moves and
// the state lands together, only while runID is the current attempt and the
// row is working; the generation predicates are RetryClaim's.
func (s *TaskStore) Advance(ctx context.Context, id, runID, nextRunID string, state json.RawMessage) (bool, error) {
	q := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("run_id = ?", nextRunID).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("run_id = ?", runID).
		Where("status = ?", string(tasks.StatusWorking)).
		Where(liveParent).Where(liveChild)
	if state != nil {
		q = q.Set("state = ?", string(state))
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("advancing task %q: %w", id, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return true, nil
	}
	return false, s.casMiss(ctx, id)
}

// ReleaseRetryClaim implements tasks.Store as one conditional UPDATE bound to
// the claimed run id, like Finalize. The attempt rolls back: the claimed run
// never launched, and attempt counts runs the task has HAD (floor as AttemptNo).
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
	if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
		return true, nil
	}
	return false, s.casMiss(ctx, id)
}

// MarkInputRequired implements tasks.Store, bound to the current attempt like
// Finalize: an approval that outlived its attempt must not pause the newer
// one. Best-effort CAS: a concurrent terminal transition wins.
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
	return false, s.casMiss(ctx, id)
}

// FailOrphans implements tasks.Store as ONE statement, so the rows reported are
// exactly the rows failed, scoped by liveParent (spec §2.13). input_required
// rows are kept: their pending approval resumes the run.
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
