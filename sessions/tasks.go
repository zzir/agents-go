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
	ParentRunID     string `bun:"parent_run_id"`
	ToolCallID      string `bun:"tool_call_id"`
	ChildSessionID  string `bun:"child_session_id,notnull"`
	Depth           int    `bun:"depth,notnull"`

	Inherit string `bun:"inherit"`

	Status      string `bun:"status,notnull"`
	NotifyState string `bun:"notify_state"`
	Summary     string `bun:"summary"`
	Result      string `bun:"result"`

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
		Status:          tasks.Status(r.Status),
		NotifyState:     tasks.NotifyState(r.NotifyState),
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
		Inherit:         string(t.Inherit),
		Status:          string(t.Status),
		NotifyState:     string(t.NotifyState),
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
func CreateTaskSchema(ctx context.Context, db *bun.DB) error {
	if _, err := db.NewCreateTable().Model((*taskRow)(nil)).IfNotExists().Exec(ctx); err != nil {
		return err
	}
	// Listing a parent's tasks and finding a task by its child session are the
	// two lookups on every run boundary; without these they are table scans.
	for name, cols := range map[string][]string{
		"idx_agent_tasks_parent": {"parent_session_id"},
		"idx_agent_tasks_child":  {"child_session_id"},
		"idx_agent_tasks_notify": {"notify_state"},
	} {
		if _, err := db.NewCreateIndex().Model((*taskRow)(nil)).
			Index(name).Column(cols...).IfNotExists().Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// terminalSet is the SQL fragment matching terminal statuses.
const terminalSet = "('completed', 'failed', 'cancelled')"

// Create implements tasks.Store.
func (s *TaskStore) Create(ctx context.Context, t *tasks.Task) error {
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	if _, err := s.db.NewInsert().Model(rowFrom(t)).Exec(ctx); err != nil {
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
	if err := s.db.NewSelect().Model(row).Where("child_session_id = ?", sessionID).Scan(ctx); err != nil {
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
		return q.Where("parent_session_id = ?", parentSessionID).OrderExpr("created_at DESC")
	})
}

// ListPendingNotify implements tasks.Store, oldest first — the notification
// lists them in the order they finished.
func (s *TaskStore) ListPendingNotify(ctx context.Context, parentSessionID string) ([]tasks.Task, error) {
	return s.query(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("parent_session_id = ?", parentSessionID).
			Where("notify_state = ?", string(tasks.NotifyPending)).
			OrderExpr("updated_at ASC")
	})
}

// ListNonTerminal implements tasks.Store.
func (s *TaskStore) ListNonTerminal(ctx context.Context, parentSessionID string) ([]tasks.Task, error) {
	return s.query(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("parent_session_id = ?", parentSessionID).
			Where("status NOT IN " + terminalSet)
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
// Status, result and the wake-up debt land together, and only while the row is
// still non-terminal. Writing them separately would let a reader see a terminal
// task whose result has not arrived — which is exactly what task_status must be
// able to rely on.
func (s *TaskStore) Finalize(ctx context.Context, id string, st tasks.Status, summary, result string) (bool, error) {
	q := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(st)).
		Set("notify_state = ?", string(tasks.NotifyPending)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status NOT IN " + terminalSet)
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

// MarkInputRequired implements tasks.Store. Best-effort CAS: a concurrent
// terminal transition wins.
func (s *TaskStore) MarkInputRequired(ctx context.Context, id string) error {
	_, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusInputRequired)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status = ?", string(tasks.StatusWorking)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("marking task %q input_required: %w", id, err)
	}
	return nil
}

// ReclaimWorking implements tasks.Store.
func (s *TaskStore) ReclaimWorking(ctx context.Context, id string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusWorking)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status = ?", string(tasks.StatusInputRequired)).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("reclaiming task %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ConsumeNotify and MarkNotifyDelivered deliberately leave updated_at alone:
// for a terminal task it is when the task FINISHED (created→updated is the
// duration a UI shows), and delivery can happen much later.
func (s *TaskStore) ConsumeNotify(ctx context.Context, id string) error {
	return s.setNotify(ctx, id, tasks.NotifyConsumed)
}

// MarkNotifyDelivered implements tasks.Store.
func (s *TaskStore) MarkNotifyDelivered(ctx context.Context, id string) error {
	return s.setNotify(ctx, id, tasks.NotifyDelivered)
}

func (s *TaskStore) setNotify(ctx context.Context, id string, to tasks.NotifyState) error {
	_, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("notify_state = ?", string(to)).
		Where("id = ?", id).
		Where("notify_state = ?", string(tasks.NotifyPending)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting task %q notify state: %w", id, err)
	}
	return nil
}

// PendingNotifyParents implements tasks.Store.
func (s *TaskStore) PendingNotifyParents(ctx context.Context) ([]string, error) {
	var ids []string
	if err := s.db.NewSelect().Model((*taskRow)(nil)).
		ColumnExpr("DISTINCT parent_session_id").
		Where("notify_state = ?", string(tasks.NotifyPending)).
		Scan(ctx, &ids); err != nil {
		return nil, fmt.Errorf("listing notify-pending parents: %w", err)
	}
	return ids, nil
}

// FailOrphans implements tasks.Store.
//
// input_required rows are kept: their pending approval persists and resumes
// the run, so they are not orphans.
func (s *TaskStore) FailOrphans(ctx context.Context) (int64, error) {
	res, err := s.db.NewUpdate().Model((*taskRow)(nil)).
		Set("status = ?", string(tasks.StatusFailed)).
		Set("summary = ?", "the process restarted while the task was running").
		// The failure is news the parent never heard.
		Set("notify_state = ?", string(tasks.NotifyPending)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("status = ?", string(tasks.StatusWorking)).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("failing orphaned tasks: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Delete implements tasks.Store.
func (s *TaskStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.NewDelete().Model((*taskRow)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return fmt.Errorf("deleting task %q: %w", id, err)
	}
	return nil
}

var _ tasks.Store = (*TaskStore)(nil)
