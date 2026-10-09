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
	// StatusInputRequired is paused for a human; not terminal — see spec §2.13.
	StatusInputRequired Status = "input_required"
	// StatusCompleted finished with a result.
	StatusCompleted Status = "completed"
	// StatusFailed finished with an error.
	StatusFailed Status = "failed"
	// StatusCancelled was stopped, by a person or by a session teardown.
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether the status is final; input_required is not.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Task is one background job: ID is the durable entity, RunID the current
// attempt at it — see spec §2.13.
type Task struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	Label string `json:"label"`
	// Kind names the job in the host's vocabulary, opaque to the SDK; empty is
	// a plain sub-agent task.
	Kind string `json:"kind,omitzero"`

	ParentSessionID string `json:"parent_session_id"`
	ParentRunID     string `json:"parent_run_id,omitzero"`
	// ToolCallID is the spawn_task call in the parent turn, the key of its UI card.
	ToolCallID     string `json:"tool_call_id,omitzero"`
	ChildSessionID string `json:"child_session_id"`

	// Depth is how many task hops from a user-initiated run; it bounds recursion.
	Depth int `json:"depth,omitzero"`

	// Attempt counts the task's runs, the original included; zero reads as 1
	// (AttemptNo).
	Attempt int `json:"attempt,omitzero"`

	// Inherit is configuration snapshotted at spawn and handed to the Launcher
	// verbatim, opaque to the SDK — see spec §2.13.
	Inherit json.RawMessage `json:"inherit,omitzero"`
	// State is the host's record of a multi-run job, opaque to the SDK;
	// Store.Advance replaces it.
	State json.RawMessage `json:"state,omitzero"`

	Status Status `json:"status"`

	// Summary is Result truncated, for the notification and the card;
	// task_status fetches Result.
	Summary string `json:"summary,omitzero"`
	Result  string `json:"result,omitzero"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AttemptNo is Attempt with the zero value read as 1.
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
	// State is Task.State, for Config.DescribeState.
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
