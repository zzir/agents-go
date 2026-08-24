package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/zzir/agents-go/agents/tasks"
)

// TaskAdapter presents this server's task rows as an agents/tasks Store: the
// SDK owns the lifecycle state machine, this server owns delivery — each
// terminal transition writes the wake-up debt the parent is owed in the same
// transaction (the host obligation tasks.Store.Finalize documents) — and the
// row keeps the columns this server's API and UI already expose.
type TaskAdapter struct {
	store *TaskStore
}

// NewTaskAdapter wraps a TaskStore as a tasks.Store.
func NewTaskAdapter(s *TaskStore) *TaskAdapter { return &TaskAdapter{store: s} }

// Inherit is the configuration snapshot the SDK carries opaquely and hands back
// when it launches a run — this server's agent config and sandbox.
type Inherit struct {
	// AgentConfigID, SandboxID and ProjectID are the SPAWNING run's setup,
	// replayed when the parent is woken so the notification reaches the agent
	// that asked for the task.
	AgentConfigID string `json:"agent_config_id,omitempty"`
	SandboxID     string `json:"sandbox_id,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
	// TaskAgentID is the agent the task itself runs as, which is usually a
	// different one.
	TaskAgentID string `json:"task_agent_id,omitempty"`
}

// DecodeInherit reads an Inherit payload, returning the zero value for an empty
// or unreadable one: a task whose snapshot cannot be read still has to be
// reportable, and the caller falls back to the session's own configuration.
func DecodeInherit(raw json.RawMessage) Inherit {
	var i Inherit
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &i)
	}
	return i
}

// EncodeInherit builds the payload stored on a task.
func EncodeInherit(i Inherit) json.RawMessage {
	b, err := json.Marshal(i)
	if err != nil {
		return nil
	}
	return b
}

// toSDK converts a row to the SDK's task.
func toSDK(t *Task) *tasks.Task {
	return &tasks.Task{
		ID:              t.ID,
		RunID:           t.RunID,
		Label:           t.Label,
		Kind:            t.Kind,
		State:           t.State,
		ParentSessionID: t.ParentSessionID,
		ParentRunID:     t.ParentRunID,
		ToolCallID:      t.ToolCallID,
		ChildSessionID:  t.ChildSessionID,
		Depth:           t.Depth,
		Attempt:         t.Attempt,
		// All three, including the task's own agent: a retry launches from the
		// snapshot read back off the row, not from a fresh resolve, so an
		// Inherit that lost TaskAgentID here would start the new attempt with
		// no agent config at all.
		Inherit: EncodeInherit(Inherit{
			AgentConfigID: t.ParentAgentConfigID,
			SandboxID:     t.ParentSandboxID,
			ProjectID:     t.ParentProjectID,
			TaskAgentID:   t.AgentConfigID,
		}),
		Status:    tasks.Status(t.Status),
		Summary:   t.Summary,
		Result:    t.Result,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func toSDKSlice(in []Task) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for i := range in {
		out = append(out, *toSDK(&in[i]))
	}
	return out
}

// Create implements tasks.Store.
func (a *TaskAdapter) Create(ctx context.Context, t *tasks.Task) error {
	inherit := DecodeInherit(t.Inherit)
	row := &Task{
		ID:              t.ID,
		RunID:           t.RunID,
		Label:           t.Label,
		Kind:            t.Kind,
		State:           t.State,
		ParentSessionID: t.ParentSessionID,
		ParentRunID:     t.ParentRunID,
		ToolCallID:      t.ToolCallID,
		ChildSessionID:  t.ChildSessionID,
		Depth:           t.Depth,
		// AgentConfigID is the task's own agent; ParentAgentConfigID is the
		// snapshot the wake-up run uses. They differ whenever a task runs as a
		// different agent than the one that spawned it, which is the usual case.
		AgentConfigID:       inherit.TaskAgentID,
		ParentAgentConfigID: inherit.AgentConfigID,
		ParentSandboxID:     inherit.SandboxID,
		ParentProjectID:     inherit.ProjectID,
		Attempt:             t.Attempt,
		Status:              string(t.Status),
		Summary:             t.Summary,
		Result:              t.Result,
	}
	if err := a.store.Create(ctx, row); err != nil {
		return err
	}
	t.CreatedAt, t.UpdatedAt = row.CreatedAt, row.UpdatedAt
	return nil
}

// Get implements tasks.Store.
func (a *TaskAdapter) Get(ctx context.Context, id string) (*tasks.Task, error) {
	row, err := a.store.Get(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return toSDK(row), nil
}

// ByChildSession implements tasks.Store.
func (a *TaskAdapter) ByChildSession(ctx context.Context, sessionID string) (*tasks.Task, error) {
	row, err := a.store.ByChildSession(ctx, sessionID)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return toSDK(row), nil
}

// ListByParent implements tasks.Store.
func (a *TaskAdapter) ListByParent(ctx context.Context, parentSessionID string) ([]tasks.Task, error) {
	rows, err := a.store.ListByParent(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	return toSDKSlice(rows), nil
}

// Finalize implements tasks.Store. The closure builds the wake-up the finished
// task owes its parent from the row the store reads inside the SAME
// transaction as the terminal write — the delivery cannot be lost to a crash
// between them, nor to a failed pre-read.
func (a *TaskAdapter) Finalize(ctx context.Context, id, runID string, st tasks.Status, summary, result string, state json.RawMessage) (bool, error) {
	return a.store.Finalize(ctx, id, runID, string(st), summary, result, state, func(row *Task) *Wakeup {
		return taskWakeup(row, st, summary, result, runID)
	})
}

// taskWakeup is the debt a finished task owes its parent, or nil when none is:
// only a completed or failed task wakes the conversation — a cancellation was
// the person's own doing and restating it would just repeat them. The payload
// is formatted through the SDK's own formatter so the wire text cannot drift
// from what task_status parses.
func taskWakeup(row *Task, st tasks.Status, summary, result, attempt string) *Wakeup {
	if st != tasks.StatusCompleted && st != tasks.StatusFailed {
		return nil
	}
	t := toSDK(row)
	t.Status, t.Summary, t.Result = st, summary, result
	return &Wakeup{
		SessionID: row.ParentSessionID,
		Kind:      WakeKindTask,
		SourceID:  row.ID,
		// The DELIVERY configuration only — no TaskAgentID: the wake run never
		// uses it, and the drain groups debts by this string, so a stray field
		// would split one wake turn into one per task agent.
		Inherit: string(EncodeInherit(Inherit{
			AgentConfigID: row.ParentAgentConfigID,
			SandboxID:     row.ParentSandboxID,
			ProjectID:     row.ParentProjectID,
		})),
		ParentRunID: row.ParentRunID,
		Payload:     tasks.DefaultNotifyFormatter([]tasks.Task{*t}),
		Attempt:     attempt,
	}
}

// Advance implements tasks.Store.
func (a *TaskAdapter) Advance(ctx context.Context, id, runID, nextRunID string, state json.RawMessage) (bool, error) {
	won, err := a.store.Advance(ctx, id, runID, nextRunID, state)
	return won, mapNotFound(err)
}

// RetryClaim implements tasks.Store.
func (a *TaskAdapter) RetryClaim(ctx context.Context, id, newRunID string, maxAttempts int) (bool, error) {
	won, err := a.store.RetryClaim(ctx, id, newRunID, maxAttempts)
	return won, mapNotFound(err)
}

// ReleaseRetryClaim implements tasks.Store. A retry that never launched leaves
// the task failed again — so it owes its parent a fresh failure notification,
// written in the same tx as the status (the prior debt was cancelled at claim).
func (a *TaskAdapter) ReleaseRetryClaim(ctx context.Context, id, runID, summary, result string) (bool, error) {
	won, err := a.store.ReleaseRetryClaim(ctx, id, runID, summary, result, func(row *Task) *Wakeup {
		return taskWakeup(row, tasks.StatusFailed, summary, result, runID)
	})
	return won, mapNotFound(err)
}

// MarkInputRequired implements tasks.Store.
func (a *TaskAdapter) MarkInputRequired(ctx context.Context, id, runID string) error {
	return a.store.MarkInputRequired(ctx, id, runID)
}

// ReclaimWorking implements tasks.Store.
func (a *TaskAdapter) ReclaimWorking(ctx context.Context, id, runID string) (bool, error) {
	won, err := a.store.ReclaimWorking(ctx, id, runID)
	return won, mapNotFound(err)
}

// FailOrphans implements tasks.Store.
func (a *TaskAdapter) FailOrphans(ctx context.Context) ([]tasks.Task, error) {
	// Each orphan's wake-up lands in the same transaction as its failure, so a
	// second crash mid-sweep cannot fail a task and forget its parent.
	rows, err := a.store.FailOrphans(ctx, func(row *Task) *Wakeup {
		return taskWakeup(row, tasks.StatusFailed, row.Summary, row.Result, row.RunID)
	})
	if err != nil {
		return nil, err
	}
	return toSDKSlice(rows), nil
}

// ListNonTerminal implements tasks.Store.
func (a *TaskAdapter) ListNonTerminal(ctx context.Context, parentSessionID string) ([]tasks.Task, error) {
	rows, err := a.store.ListNonTerminalByParent(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	return toSDKSlice(rows), nil
}

// Delete implements tasks.Store.
func (a *TaskAdapter) Delete(ctx context.Context, id string) error {
	return a.store.DeleteByID(ctx, id)
}

// mapNotFound translates the server's not-found into the SDK's, so a caller can
// use one errors.Is regardless of which layer answered.
func mapNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return tasks.ErrNotFound
	}
	return err
}

var _ tasks.Store = (*TaskAdapter)(nil)
