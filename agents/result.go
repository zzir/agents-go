package agents

import (
	"fmt"

	"github.com/zzir/agents-go/agents/session"
)

// RunResult is the outcome of a completed (non-streaming) run.
type RunResult struct {
	// Input is the first model call's input: session history followed by the
	// new user input, as a handoff input filter may have rewritten it.
	Input []InputItem
	// NewItems are all items generated during the run.
	NewItems []*RunItem
	// RawResponses are the raw model responses, in order.
	RawResponses []*ModelResponse
	// FinalOutput is a string for plain-text agents, the decoded value for an
	// OutputType.
	FinalOutput any
	// LastAgent is the agent that produced the final output (after any handoffs).
	LastAgent *Agent
	// Usage is the run's aggregated token usage, a snapshot taken when the
	// result was built; a later resume cannot change it.
	Usage *Usage
	// GuardrailResults holds every guardrail result of the run, allowing
	// decisions included; filter with GuardrailResult.Stage.
	GuardrailResults []GuardrailResult
	// Interruptions holds pending tool approvals when a run pauses for HITL.
	Interruptions []*ToolApprovalItem
	// State is the serializable run state of a paused run: approve or reject
	// on it and resume with ResumeRun. Nil when the run completed.
	State *RunState
	// Diagnostics records trouble the run survived — see spec §2.11d.
	Diagnostics []Diagnostic

	// AgentToolInvocation identifies the parent tool call of a nested
	// agent-as-tool run; nil for top-level runs.
	AgentToolInvocation *AgentToolInvocation

	// StoppedEarly reports that RunControl.StopAfterTurn ended the run rather
	// than the agent finishing — see spec §2.12.
	StoppedEarly bool
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

// ToolApprovalItem is a tool call awaiting human approval, listed in
// RunResult.Interruptions; approve or reject it on a RunState and ResumeRun.
type ToolApprovalItem struct {
	Agent    *Agent
	ToolName string
	CallID   string
	// Arguments is the raw JSON arguments string the model emitted.
	Arguments string
	// Raw is the model's tool-call output item, re-processed on resume.
	Raw OutputItem
}

// ToInputList returns the run's whole conversation as model input: the input
// it started from followed by everything it produced.
func (r *RunResult) ToInputList() ([]InputItem, error) {
	return buildModelInput(r.Input, r.NewItems)
}

// UsageByResponse breaks the run's usage down per model call, keyed by response id.
func (r *RunResult) UsageByResponse() map[string]RequestUsage {
	out := make(map[string]RequestUsage, len(r.RawResponses))
	for _, resp := range r.RawResponses {
		if resp == nil || resp.Usage == nil || resp.ResponseID == "" {
			continue
		}
		u := resp.Usage.Request()
		// A retried or resumed run can see the same response id twice: sum,
		// never overwrite.
		if prev, ok := out[resp.ResponseID]; ok {
			session.AddRequestUsage(&u, &prev)
		}
		out[resp.ResponseID] = u
	}
	return out
}

// NestedUsage totals what the run's tools spent on model calls of their own
// (agent-as-tool sub-runs, summarization); already part of Usage — see spec §2.7f.
func (r *RunResult) NestedUsage() RequestUsage {
	var total RequestUsage
	for _, it := range r.NewItems {
		if it.Kind != ItemToolCallOutput || it.NestedUsage == nil {
			continue
		}
		u := it.NestedUsage.Request()
		session.AddRequestUsage(&total, &u)
	}
	return total
}
