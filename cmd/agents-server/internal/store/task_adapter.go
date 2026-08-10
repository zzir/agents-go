package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/zzir/agents-go/agents/tasks"
)

// TaskAdapter presents this server's task rows as an agents/tasks Store, so the
// SDK owns the lifecycle — the state machine, the wake-up debt, the guards —
// while the row keeps the columns this server's API and UI already expose.
//
// The alternative was adopting the SDK's own SQL store, which would have meant
// depending on the sessions module and reshaping the REST payload the frontend
// reads. Neither buys anything: what was worth moving is the 669 lines of
// logic, not the table.
type TaskAdapter struct {
	store *TaskStore
}

// NewTaskAdapter wraps a TaskStore as a tasks.Store.
func NewTaskAdapter(s *TaskStore) *TaskAdapter { return &TaskAdapter{store: s} }

// Inherit is the configuration snapshot the SDK carries opaquely and hands back
// when it launches a run — this server's agent config and sandbox.
type Inherit struct {
	// AgentConfigID, SandboxID and WorkDir are the SPAWNING run's setup,
	// replayed when the parent is woken so the notification reaches the agent
	// that asked for the task.
	AgentConfigID string `json:"agent_config_id,omitempty"`
	SandboxID     string `json:"sandbox_id,omitempty"`
	WorkDir       string `json:"work_dir,omitempty"`
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
			WorkDir:       t.ParentWorkDir,
			TaskAgentID:   t.AgentConfigID,
		}),
		Status:      tasks.Status(t.Status),
		NotifyState: tasks.NotifyState(t.NotifyState),
		Summary:     t.Summary,
		Result:      t.Result,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
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
		ParentWorkDir:       inherit.WorkDir,
		Attempt:             t.Attempt,
		Status:              string(t.Status),
		NotifyState:         string(t.NotifyState),
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

// Finalize implements tasks.Store.
func (a *TaskAdapter) Finalize(ctx context.Context, id, runID string, st tasks.Status, summary, result string) (bool, error) {
	return a.store.Finalize(ctx, id, runID, string(st), summary, result)
}

// RetryClaim implements tasks.Store.
func (a *TaskAdapter) RetryClaim(ctx context.Context, id, newRunID string, maxAttempts int) (bool, error) {
	won, err := a.store.RetryClaim(ctx, id, newRunID, maxAttempts)
	return won, mapNotFound(err)
}

// ReleaseRetryClaim implements tasks.Store.
func (a *TaskAdapter) ReleaseRetryClaim(ctx context.Context, id, runID, summary, result string) (bool, error) {
	won, err := a.store.ReleaseRetryClaim(ctx, id, runID, summary, result)
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

// ConsumeNotify implements tasks.Store.
func (a *TaskAdapter) ConsumeNotify(ctx context.Context, id, runID string) error {
	return a.store.ConsumeNotify(ctx, id, runID)
}

// MarkNotifyDelivered implements tasks.Store.
func (a *TaskAdapter) MarkNotifyDelivered(ctx context.Context, id, runID string) error {
	return a.store.MarkNotifyDelivered(ctx, id, runID)
}

// ListPendingNotify implements tasks.Store.
func (a *TaskAdapter) ListPendingNotify(ctx context.Context, parentSessionID string) ([]tasks.Task, error) {
	rows, err := a.store.ListPendingNotify(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	return toSDKSlice(rows), nil
}

// PendingNotifyParents implements tasks.Store.
func (a *TaskAdapter) PendingNotifyParents(ctx context.Context) ([]string, error) {
	return a.store.PendingNotifyParents(ctx)
}

// FailOrphans implements tasks.Store.
func (a *TaskAdapter) FailOrphans(ctx context.Context) (int64, error) {
	return a.store.FailOrphans(ctx)
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
