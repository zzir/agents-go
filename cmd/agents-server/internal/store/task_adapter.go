package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	AgentConfigID string `json:"agent_config_id,omitempty"`
	SandboxID     string `json:"sandbox_id,omitempty"`
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
		Inherit: EncodeInherit(Inherit{
			AgentConfigID: t.ParentAgentConfigID,
			SandboxID:     t.ParentSandboxID,
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
		// ParentAgentConfigID is the snapshot the wake-up run uses. The task's
		// own agent is written separately by SetTaskAgentConfig: it is this
		// server's notion, not the SDK's, and it does not belong in a shared
		// type.
		ParentAgentConfigID: inherit.AgentConfigID,
		ParentSandboxID:     inherit.SandboxID,
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
func (a *TaskAdapter) Finalize(ctx context.Context, id string, st tasks.Status, summary, result string) (bool, error) {
	return a.store.Finalize(ctx, id, string(st), summary, result)
}

// MarkInputRequired implements tasks.Store.
func (a *TaskAdapter) MarkInputRequired(ctx context.Context, id string) error {
	return a.store.MarkInputRequired(ctx, id)
}

// ReclaimWorking implements tasks.Store.
func (a *TaskAdapter) ReclaimWorking(ctx context.Context, id string) (bool, error) {
	return a.store.ReclaimWorking(ctx, id)
}

// ConsumeNotify implements tasks.Store.
func (a *TaskAdapter) ConsumeNotify(ctx context.Context, id string) error {
	return a.store.ConsumeNotify(ctx, id)
}

// MarkNotifyDelivered implements tasks.Store.
func (a *TaskAdapter) MarkNotifyDelivered(ctx context.Context, id string) error {
	return a.store.MarkNotifyDelivered(ctx, id)
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
	var rows []Task
	if err := a.store.db.NewSelect().Model(&rows).
		Where("parent_session_id = ?", parentSessionID).
		Where("status NOT IN " + taskTerminalSet).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing live tasks for %s: %w", parentSessionID, err)
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

// SetTaskAgentConfig records the task's own agent config after creation. The
// SDK's model has no field for it — it is this server's notion — so it rides in
// through a follow-up write rather than distorting the shared type.
func (a *TaskAdapter) SetTaskAgentConfig(ctx context.Context, id, agentConfigID string) error {
	if agentConfigID == "" {
		return nil
	}
	_, err := a.store.db.NewUpdate().Model((*Task)(nil)).
		Set("agent_config_id = ?", agentConfigID).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

var _ tasks.Store = (*TaskAdapter)(nil)
