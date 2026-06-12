package agents

import "fmt"

// RunResult is the outcome of a completed (non-streaming) run. It mirrors the
// Python SDK's RunResult.
type RunResult struct {
	// Input is the input list of the run's first model call: the session
	// history (when a Session is configured) followed by the new user input.
	// A handoff input filter may have rewritten it. This matches the Python
	// SDK's RunResult.input semantics; the items passed to Run itself are not
	// retained separately.
	Input []TResponseInputItem
	// NewItems are all items generated during the run (messages, tool calls,
	// tool outputs, handoffs, reasoning).
	NewItems []RunItem
	// RawResponses are the raw model responses, in order.
	RawResponses []*ModelResponse
	// FinalOutput is the final output value. For plain-text agents it is a
	// string; for agents with an OutputType it is the decoded value.
	FinalOutput any
	// LastAgent is the agent that produced the final output (after any handoffs).
	LastAgent *Agent
	// Usage is the aggregated token usage across the run.
	Usage *Usage
	// Interruptions holds pending tool approvals when a run pauses for HITL.
	// It is empty for runs that complete normally.
	Interruptions []*ToolApprovalItem
	// State is the serializable run state captured when the run pauses for
	// approvals. Approve/reject items on it and resume with ResumeRun. It is nil
	// for runs that complete normally.
	State *RunState
}

// FinalOutputString returns the final output as a string when the agent produced
// plain text. For structured outputs, use a type assertion on FinalOutput.
func (r *RunResult) FinalOutputString() string {
	if s, ok := r.FinalOutput.(string); ok {
		return s
	}
	if r.FinalOutput == nil {
		return ""
	}
	return fmt.Sprintf("%v", r.FinalOutput)
}

// FinalOutputAs decodes the final output into a value of type T. It succeeds
// when the run's agent used OutputType[T] (or a compatible type).
func FinalOutputAs[T any](r *RunResult) (T, bool) {
	v, ok := r.FinalOutput.(T)
	return v, ok
}

// ToolApprovalItem represents a tool call awaiting human approval (HITL). When a
// run pauses, these appear in RunResult.Interruptions; approve or reject them on
// a RunState and resume with ResumeRun.
type ToolApprovalItem struct {
	Agent    *Agent
	ToolName string
	CallID   string
	// Arguments is the raw JSON arguments string the model emitted.
	Arguments string
	// Raw is the underlying model tool-call output item, retained so the run can
	// re-process it on resume.
	Raw TResponseOutputItem
}
