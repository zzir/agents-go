// Package tasks runs background sub-agents: a tool call spawns a child run with
// its own session, the parent run does not wait, and the parent is woken with
// the result when the child finishes.
//
// It is a general orchestration pattern rather than a product feature, and the
// reason it lives in the SDK is everything in this file's neighbourhood: the
// state machine, the wake-up debt that survives a restart, the four guards that
// decide when NOT to wake a parent, the compare-and-set that keeps two
// finalizers from overwriting each other. Anyone building "hand work to a
// background agent" has to solve all of it, and each one is a bug before it is
// a design.
package tasks

import (
	"encoding/json"
	"time"
)

// Status is where a task is.
type Status string

const (
	// StatusWorking is running.
	StatusWorking Status = "working"
	// StatusInputRequired is paused for a human — NOT terminal. The approval
	// flow surfaces it, and the resumed run lands back on a terminal status.
	StatusInputRequired Status = "input_required"
	// StatusCompleted finished with a result.
	StatusCompleted Status = "completed"
	// StatusFailed finished with an error.
	StatusFailed Status = "failed"
	// StatusCancelled was stopped, by a person or by a session teardown.
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether the status is final.
//
// input_required is deliberately not terminal: a task waiting on a human is
// still a task in flight, and treating it as finished would deliver a
// notification for something that has not happened.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// NotifyState is the durable state machine for the wake-up a finished task owes
// its parent session.
//
// It is persisted rather than held in memory because the debt has to survive a
// restart: a task that finished while the parent was busy, and a process that
// died before delivering, are the same situation from the parent's side — it
// was never told.
type NotifyState string

const (
	// NotifyNone means no debt yet: the task has not finished.
	NotifyNone NotifyState = ""
	// NotifyPending means the terminal state is written and a wake-up is owed.
	NotifyPending NotifyState = "pending"
	// NotifyConsumed means the model already pulled the result in-turn with
	// task_status, so waking it to repeat the news would burn a turn.
	NotifyConsumed NotifyState = "consumed"
	// NotifyDelivered means a wake-up run carried the result to the parent.
	NotifyDelivered NotifyState = "delivered"
)

// Task is one background job.
//
// ID and RunID are separate on purpose: the task is the durable entity and the
// run is one attempt at it. Collapsing them would make a retry impossible to
// express without inventing a second task.
type Task struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	Label string `json:"label"`

	ParentSessionID string `json:"parent_session_id"`
	ParentRunID     string `json:"parent_run_id,omitzero"`
	// ToolCallID is the spawn_task call in the parent turn, so a UI card can be
	// updated when the task finishes — long after that turn ended.
	ToolCallID     string `json:"tool_call_id,omitzero"`
	ChildSessionID string `json:"child_session_id"`

	// Depth is how many task hops from a user-initiated run. It bounds
	// recursion: a task that can spawn tasks can spawn them forever.
	Depth int `json:"depth,omitzero"`

	// Inherit is configuration snapshotted from the spawning run and handed
	// back to the Launcher verbatim — agent config, sandbox, tenant. The SDK
	// does not interpret it.
	//
	// It is a snapshot rather than a lookup because the wake-up run happens
	// much later, and the parent's configuration may have changed or gone. A
	// notification delivered under a different agent than the one that spawned
	// the task is a confusing thing to receive.
	Inherit json.RawMessage `json:"inherit,omitzero"`

	Status      Status      `json:"status"`
	NotifyState NotifyState `json:"notify_state,omitzero"`

	// Summary is the truncated result: it goes in the notification and on the
	// UI card. Result is the whole thing, fetched on demand by task_status.
	//
	// They are separate so a task returning ten thousand words does not paste
	// them into the parent's context to say "done".
	Summary string `json:"summary,omitzero"`
	Result  string `json:"result,omitzero"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Info is the public view of a task, returned by the tools.
type Info struct {
	TaskID  string `json:"task_id"`
	Label   string `json:"label,omitzero"`
	Agent   string `json:"agent,omitzero"`
	Status  Status `json:"status"`
	Summary string `json:"summary,omitzero"`
	Result  string `json:"result,omitzero"`
}

func infoFrom(t *Task, agent string) *Info {
	return &Info{
		TaskID:  t.ID,
		Label:   t.Label,
		Agent:   agent,
		Status:  t.Status,
		Summary: t.Summary,
		Result:  t.Result,
	}
}
