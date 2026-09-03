// Package tasks runs background sub-agents: a tool call spawns a child run with
// its own session, the parent run does not wait, and the parent is woken with
// the result when the child finishes. The invariants: spec §2.13.
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

// Terminal reports whether the status is final. input_required is deliberately
// not terminal: a task waiting on a human is still in flight.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Task is one background job. ID and RunID are separate: the task is the
// durable entity, the run one attempt at it (spec §2.13). A job may span
// several runs (Config.Continue); RunID is always the current one.
type Task struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	Label string `json:"label"`
	// Kind names what sort of job this is, in the host's vocabulary; the SDK
	// does not interpret it. Empty is a plain sub-agent task.
	Kind string `json:"kind,omitzero"`

	ParentSessionID string `json:"parent_session_id"`
	ParentRunID     string `json:"parent_run_id,omitzero"`
	// ToolCallID is the spawn_task call in the parent turn, so a UI card can be
	// updated when the task finishes — long after that turn ended.
	ToolCallID     string `json:"tool_call_id,omitzero"`
	ChildSessionID string `json:"child_session_id"`

	// Depth is how many task hops from a user-initiated run; it bounds recursion.
	Depth int `json:"depth,omitzero"`

	// Attempt counts the runs this task has had: 1 for the original, one more
	// for each retry. Zero reads as 1 (see AttemptNo).
	Attempt int `json:"attempt,omitzero"`

	// Inherit is configuration snapshotted from the spawning run and handed back
	// to the Launcher verbatim, opaque to the SDK — a snapshot, since the
	// wake-up run comes much later (spec §2.13).
	Inherit json.RawMessage `json:"inherit,omitzero"`
	// State is the host's own record of where a multi-run job stands, opaque to
	// the SDK; replaced atomically with each run transition (Store.Advance).
	State json.RawMessage `json:"state,omitzero"`

	Status Status `json:"status"`

	// Summary is the truncated result, for the notification and the card;
	// Result is the whole thing, fetched on demand by task_status.
	Summary string `json:"summary,omitzero"`
	Result  string `json:"result,omitzero"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AttemptNo is the task's attempt count, reading the zero value as the first
// attempt. Every caller wants this rather than the raw field.
func (t *Task) AttemptNo() int {
	if t.Attempt <= 0 {
		return 1
	}
	return t.Attempt
}

// Info is the public view of a task, returned by the tools.
type Info struct {
	TaskID  string `json:"task_id"`
	Label   string `json:"label,omitzero"`
	Kind    string `json:"kind,omitzero"`
	Agent   string `json:"agent,omitzero"`
	Status  Status `json:"status"`
	Attempt int    `json:"attempt,omitzero"`
	Summary string `json:"summary,omitzero"`
	Result  string `json:"result,omitzero"`
	// State is the host's record of the job (Task.State), carried so a host
	// can say where a job of its kind stands (Config.DescribeState).
	State json.RawMessage `json:"state,omitzero"`
}

func infoFrom(t *Task, agent string) *Info {
	return &Info{
		TaskID:  t.ID,
		Label:   t.Label,
		Kind:    t.Kind,
		Agent:   agent,
		Status:  t.Status,
		Attempt: t.AttemptNo(),
		Summary: t.Summary,
		Result:  t.Result,
		State:   t.State,
	}
}
