package bridge

// TaskInfo is the wire shape of a background task (REST and the OpenAPI spec):
// its own struct, not agents/tasks' Info, so the SDK's view can move without
// moving the contract.
type TaskInfo struct {
	TaskID string `json:"task_id"`
	Label  string `json:"label,omitempty"`
	// Kind is "" for a sub-agent task, "workflow" for an execution.
	Kind   string `json:"kind,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Status string `json:"status"`
	// Attempt counts the task's runs: 1 for the original, one more per retry.
	Attempt int `json:"attempt,omitempty"`
	// MaxAttempts is the ceiling Attempt is measured against; a client offering
	// a retry compares the two (capacity is not part of it).
	MaxAttempts int    `json:"max_attempts,omitempty"`
	Summary     string `json:"summary,omitempty"`
	// Result carries the task's full final output (task_status only — the
	// wake notification and the UI card stay on the truncated Summary).
	Result string `json:"result,omitempty"`
}
