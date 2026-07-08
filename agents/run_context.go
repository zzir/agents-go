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

	// TurnInput is the model input for the turn currently executing: the
	// conversation the model was shown before it produced the response whose
	// tools are now running. The runner refreshes it at the start of each turn
	// so tools, guardrails and hooks can inspect the input that led to the call.
	// It mirrors the Python SDK's RunContextWrapper.turn_input.
	TurnInput []TResponseInputItem

	// inheritedOpts carries the run's model provider/model so nested runs (e.g.
	// agent-as-tool) inherit them. Set by the runner; not user-facing.
	inheritedOpts *RunOptions

	// activeTrace is the run's trace handle, so nested runs join it instead of
	// starting an orphan root trace. Set by the runner; not user-facing.
	activeTrace *tracing.TraceHandle

	// nestedToolStates caches the paused RunState of an agent-as-tool nested run,
	// keyed by the parent tool call id, so a resumed parent run continues the
	// nested run from where it paused instead of restarting it. Populated by the
	// runner at an interruption and re-installed onto the resume's context from
	// RunState. In-process only (never serialized) — matching Python's ephemeral
	// agent-tool result cache, which likewise does not survive a RunState JSON
	// round-trip (a cross-process resume restarts the nested run).
	nestedToolStates map[string]*RunState
}

// takeNestedToolState returns and removes the cached nested run state for a
// parent tool call id, if any. Used by an agent-as-tool on resume to continue
// its paused nested run.
func (c *RunContext) takeNestedToolState(callID string) *RunState {
	if c.nestedToolStates == nil {
		return nil
	}
	st, ok := c.nestedToolStates[callID]
	if !ok {
		return nil
	}
	delete(c.nestedToolStates, callID)
	return st
}

// NewRunContext returns a RunContext wrapping the given user value with a fresh
// Usage accumulator.
func NewRunContext(userData any) *RunContext {
	return &RunContext{Context: userData, Usage: NewUsage(), Approvals: NewApprovalStore()}
}

// ApprovalStore records human-in-the-loop approval decisions for tool calls. A
// decision can be scoped to a single call (by call ID) or made "always" for a
// tool name. It is goroutine-safe.
//
// Each tool name has one entry holding a permanent allow/deny plus per-call
// allow/deny sets, mirroring the Python SDK's approval model. Precedence when
// resolving a call: a permanent approval wins over everything (including a
// permanent or per-call rejection), then a permanent rejection, then a per-call
// approval, then a per-call rejection.
type ApprovalStore struct {
	mu      sync.Mutex
	entries map[string]*approvalEntry // keyed by tool name
}

// approvalEntry holds the decisions recorded for one tool name.
type approvalEntry struct {
	approvedAll   bool
	rejectedAll   bool
	approvedIDs   map[string]bool
	rejectedIDs   map[string]bool
	messages      map[string]string // per-call rejection message
	stickyMessage string            // permanent-rejection message
}

type approvalDecision struct {
	approved bool
	message  string // rejection message, when !approved
}

// NewApprovalStore returns an empty approval store.
func NewApprovalStore() *ApprovalStore {
	return &ApprovalStore{entries: map[string]*approvalEntry{}}
}

// entryFor returns the entry for a tool name, creating it if absent. Caller
// holds the lock.
func (s *ApprovalStore) entryFor(toolName string) *approvalEntry {
	e := s.entries[toolName]
	if e == nil {
		e = &approvalEntry{approvedIDs: map[string]bool{}, rejectedIDs: map[string]bool{}, messages: map[string]string{}}
		s.entries[toolName] = e
	}
	return e
}

// Approve records approval for a tool call. If always is true, every future call
// to the same tool is approved (and any prior rejections for it are cleared, so
// the permanent approval takes precedence).
func (s *ApprovalStore) Approve(item *ToolApprovalItem, always bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryFor(item.ToolName)
	if always {
		e.approvedAll = true
		e.rejectedAll = false
		e.rejectedIDs = map[string]bool{}
		e.messages = map[string]string{}
		e.stickyMessage = ""
		return
	}
	e.approvedIDs[item.CallID] = true
	delete(e.rejectedIDs, item.CallID)
	delete(e.messages, item.CallID)
}

// Reject records rejection for a tool call, optionally with a custom message
// sent back to the model. If always is true, every future call to the same tool
// is rejected.
func (s *ApprovalStore) Reject(item *ToolApprovalItem, always bool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryFor(item.ToolName)
	if always {
		e.rejectedAll = true
		e.approvedAll = false
		e.stickyMessage = message
		return
	}
	e.rejectedIDs[item.CallID] = true
	delete(e.approvedIDs, item.CallID)
	if message != "" {
		e.messages[item.CallID] = message
	} else {
		delete(e.messages, item.CallID)
	}
}

// decisionFor returns the recorded decision for a call. ok is false when the
// tool name has no entry or the call is undecided. Precedence matches Python's
// is_tool_approved: permanent approval, permanent rejection, per-call approval,
// per-call rejection.
func (s *ApprovalStore) decisionFor(toolName, callID string) (approvalDecision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[toolName]
	if e == nil {
		return approvalDecision{}, false
	}
	if e.approvedAll {
		return approvalDecision{approved: true}, true
	}
	if e.rejectedAll {
		return approvalDecision{message: e.stickyMessage}, true
	}
	if e.approvedIDs[callID] {
		return approvalDecision{approved: true}, true
	}
	if e.rejectedIDs[callID] {
		return approvalDecision{message: e.messages[callID]}, true
	}
	return approvalDecision{}, false
}

// mirrorInto copies this store's decision for each of the given approval items
// into dst, keyed by the item's call id. An agent-as-tool uses it to carry the
// parent run's approve/reject decisions into the nested run it resumes, so the
// human's choice on a surfaced nested interruption actually takes effect.
func (s *ApprovalStore) mirrorInto(dst *ApprovalStore, items []*ToolApprovalItem) {
	if dst == nil {
		return
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		if d, ok := s.decisionFor(it.ToolName, it.CallID); ok {
			if d.approved {
				dst.Approve(it, false)
			} else {
				dst.Reject(it, false, d.message)
			}
		}
	}
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
	// Agent is the agent whose tool is being invoked. It mirrors the Python
	// SDK's ToolContext.agent.
	Agent *Agent
	// ToolCall is the raw model-emitted function-call output item that triggered
	// this invocation. It mirrors the Python SDK's ToolContext.tool_call.
	ToolCall TResponseOutputItem
	// functionSpanID is the tracing span ID of this tool call, letting a
	// nested agent-as-tool run parent its agent spans under the function span
	// instead of floating at the trace root.
	functionSpanID string
}
