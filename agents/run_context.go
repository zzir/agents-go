package agents

import (
	"sync"

	"github.com/zzir/agents-go/tracing"
)

// RunContext carries user-supplied data and run-scoped state through a single
// agent run. It is passed to tool invocations, guardrails and lifecycle hooks.
//
// Unlike the Python SDK, which parameterizes Agent on a context type, the Go SDK
// keeps user data in the Context field as an any value. Tool authors type-assert
// it back to their concrete type. The standard library context.Context is used
// separately for cancellation and deadlines.
type RunContext struct {
	// Context is the arbitrary user value threaded through the run. It is never
	// inspected by the SDK.
	Context any
	// Usage accumulates token usage across every model call in the run.
	Usage *Usage
	// Approvals tracks human-in-the-loop tool approval decisions.
	Approvals *ApprovalStore

	// inheritedOpts carries the run's model provider/model so nested runs (e.g.
	// agent-as-tool) inherit them. Set by the runner; not user-facing.
	inheritedOpts *RunOptions

	// activeTrace is the run's trace handle, so nested runs join it instead of
	// starting an orphan root trace. Set by the runner; not user-facing.
	activeTrace *tracing.TraceHandle
}

// NewRunContext returns a RunContext wrapping the given user value with a fresh
// Usage accumulator.
func NewRunContext(userData any) *RunContext {
	return &RunContext{Context: userData, Usage: NewUsage(), Approvals: NewApprovalStore()}
}

// ApprovalStore records human-in-the-loop approval decisions for tool calls. A
// decision can be scoped to a single call (by call ID) or made "always" for a
// tool name. It is goroutine-safe.
type ApprovalStore struct {
	mu         sync.Mutex
	byCallID   map[string]approvalDecision
	byToolName map[string]approvalDecision
}

type approvalDecision struct {
	approved bool
	rejected bool
	message  string // rejection message, when rejected
}

// NewApprovalStore returns an empty approval store.
func NewApprovalStore() *ApprovalStore {
	return &ApprovalStore{
		byCallID:   map[string]approvalDecision{},
		byToolName: map[string]approvalDecision{},
	}
}

// Approve records approval for a tool call. If always is true, every future call
// to the same tool is approved.
func (s *ApprovalStore) Approve(item *ToolApprovalItem, always bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := approvalDecision{approved: true}
	if always {
		s.byToolName[item.ToolName] = d
	}
	s.byCallID[item.CallID] = d
}

// Reject records rejection for a tool call, optionally with a custom message
// sent back to the model. If always is true, every future call to the same tool
// is rejected.
func (s *ApprovalStore) Reject(item *ToolApprovalItem, always bool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := approvalDecision{rejected: true, message: message}
	if always {
		s.byToolName[item.ToolName] = d
	}
	s.byCallID[item.CallID] = d
}

// decisionFor returns the recorded decision for a call, checking the call ID
// first, then any "always" decision for the tool. ok is false when undecided.
func (s *ApprovalStore) decisionFor(toolName, callID string) (approvalDecision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.byCallID[callID]; ok {
		return d, true
	}
	if d, ok := s.byToolName[toolName]; ok {
		return d, true
	}
	return approvalDecision{}, false
}

// ToolContext is the context passed to a function tool when it is invoked. It
// embeds the RunContext and adds metadata about the specific tool call.
type ToolContext struct {
	*RunContext
	// ToolName is the name of the tool being invoked.
	ToolName string
	// ToolCallID is the model-assigned identifier for this tool call.
	ToolCallID string
	// ToolArguments is the raw JSON arguments string emitted by the model.
	ToolArguments string
}
