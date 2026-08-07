package bridge

// TaskInfo is this server's API shape for a background task, returned by the
// REST endpoints and recorded in the OpenAPI spec.
//
// It mirrors agents/tasks' own Info rather than reusing it, because the two
// answer to different audiences: this one is a wire contract the frontend and
// the generated spec depend on, and it should not change because the SDK's
// internal view did.
type TaskInfo struct {
	TaskID string `json:"task_id"`
	Label  string `json:"label,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Status string `json:"status"`
	// Attempt counts the task's runs: 1 for the original, one more per retry.
	Attempt int `json:"attempt,omitempty"`
	// MaxAttempts is the ceiling Attempt is measured against: a client offering
	// a retry compares the two rather than being told the answer, so its offer
	// follows the status it already tracks. Capacity is not part of it — a
	// retry can still be refused when the parent session is full.
	MaxAttempts int    `json:"max_attempts,omitempty"`
	Summary     string `json:"summary,omitempty"`
	// Result carries the task's full final output (task_status only — the
	// wake notification and the UI card stay on the truncated Summary).
	Result string `json:"result,omitempty"`
}
