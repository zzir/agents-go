package agents

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/zzir/agents-go/tracing"
)

// RunContext carries user data and run-scoped state through one run, to
// tools, guardrails and hooks — see decisions §5.12.
type RunContext struct {
	// Context is the arbitrary user value threaded through the run; the SDK
	// never inspects it.
	Context any
	// Usage accumulates token usage across the run. Live while the run
	// executes: mid-run readers go through Usage.Snapshot.
	Usage *Usage
	// Approvals tracks human-in-the-loop tool approval decisions.
	Approvals *ApprovalStore

	// turnInputMu guards turnInput: the loop refreshes it while tools read it.
	turnInputMu sync.RWMutex
	turnInput   []InputItem

	// inheritedOpts carries the run's options for nested runs to inherit. Set
	// by the runner.
	inheritedOpts *RunOptions

	// activeTrace is the run's trace handle, which nested runs join. Set by the runner.
	activeTrace *tracing.TraceHandle

	// nestedToolStates caches paused agent-as-tool states by parent call id
	// for a resume; nestedMu guards it (a resume replays tools concurrently).
	nestedMu         sync.Mutex
	nestedToolStates map[string]*RunState

	// contextReset is a model-requested context reset awaiting the save point;
	// contextFresh says the context is the last reset's, with no work since.
	contextReset atomic.Bool
	contextFresh atomic.Bool
}

// RequestContextReset asks for a fresh context window at the turn's save
// point; false while the context is still fresh from a reset — see spec §2.5i.
func (rc *RunContext) RequestContextReset() bool {
	if rc == nil || rc.contextFresh.Load() {
		return false
	}
	rc.contextReset.Store(true)
	return true
}

// ContextResetRequested reports a reset asked for and not yet performed.
func (rc *RunContext) ContextResetRequested() bool {
	return rc != nil && rc.contextReset.Load()
}

// takeContextReset consumes the request.
func (rc *RunContext) takeContextReset() bool {
	return rc != nil && rc.contextReset.Swap(false)
}

// TurnInput returns exactly what the executing turn sent the model (the delta
// under server-managed state); nil before the first turn's input is built.
// The slice is a copy whose items are shared with the live request: read-only.
func (rc *RunContext) TurnInput() []InputItem {
	if rc == nil {
		return nil
	}
	rc.turnInputMu.RLock()
	defer rc.turnInputMu.RUnlock()
	if len(rc.turnInput) == 0 {
		return nil
	}
	return append([]InputItem(nil), rc.turnInput...)
}

// setTurnInput publishes the turn's model input — see spec §2.2 step 1.
func (rc *RunContext) setTurnInput(items []InputItem) {
	if rc == nil {
		return
	}
	rc.turnInputMu.Lock()
	rc.turnInput = items
	rc.turnInputMu.Unlock()
}

// takeNestedToolState returns and removes the cached nested run state for a
// parent call id.
func (rc *RunContext) takeNestedToolState(callID string) *RunState {
	rc.nestedMu.Lock()
	defer rc.nestedMu.Unlock()
	if rc.nestedToolStates == nil {
		return nil
	}
	st, ok := rc.nestedToolStates[callID]
	if !ok {
		return nil
	}
	delete(rc.nestedToolStates, callID)
	return st
}

// NewRunContext returns a RunContext wrapping userData with a fresh Usage accumulator.
func NewRunContext(userData any) *RunContext {
	return &RunContext{Context: userData, Usage: NewUsage(), Approvals: NewApprovalStore()}
}

// ApprovalStore records human-in-the-loop approval decisions, per call id or
// "always" per tool name; a call's own decision outranks "always" — see spec §2.7.
// Goroutine-safe.
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

// forget drops the decision recorded for one call.
func (e *approvalEntry) forget(callID string) {
	delete(e.approvedIDs, callID)
	delete(e.rejectedIDs, callID)
	delete(e.messages, callID)
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

// Approve records approval for a tool call; always approves every call to the
// tool without a decision of its own, replacing an "always" rejection.
func (s *ApprovalStore) Approve(item *ToolApprovalItem, always bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryFor(item.ToolName)
	if always {
		e.approvedAll = true
		e.rejectedAll = false
		e.stickyMessage = ""
		e.forget(item.CallID)
		return
	}
	e.approvedIDs[item.CallID] = true
	delete(e.rejectedIDs, item.CallID)
	delete(e.messages, item.CallID)
}

// Reject records rejection for a tool call, with an optional message for the
// model; always rejects every call to the tool without a decision of its own.
func (s *ApprovalStore) Reject(item *ToolApprovalItem, always bool, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryFor(item.ToolName)
	if always {
		e.rejectedAll = true
		e.approvedAll = false
		e.stickyMessage = message
		e.forget(item.CallID)
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

// decisionFor returns the recorded decision for a call; ok is false when undecided.
func (s *ApprovalStore) decisionFor(toolName, callID string) (approvalDecision, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[toolName]
	if e == nil {
		return approvalDecision{}, false
	}
	if e.approvedIDs[callID] {
		return approvalDecision{approved: true}, true
	}
	if e.rejectedIDs[callID] {
		return approvalDecision{message: e.messages[callID]}, true
	}
	if e.approvedAll {
		return approvalDecision{approved: true}, true
	}
	if e.rejectedAll {
		return approvalDecision{message: e.stickyMessage}, true
	}
	return approvalDecision{}, false
}

// snapshot lifts every decision out in serialized form, call-id lists sorted
// for stable bytes.
func (s *ApprovalStore) snapshot() map[string]serialApprovalEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]serialApprovalEntry, len(s.entries))
	for tool, e := range s.entries {
		se := serialApprovalEntry{
			ApprovedAll:   e.approvedAll,
			RejectedAll:   e.rejectedAll,
			ApprovedIDs:   slices.Sorted(maps.Keys(e.approvedIDs)),
			RejectedIDs:   slices.Sorted(maps.Keys(e.rejectedIDs)),
			StickyMessage: e.stickyMessage,
		}
		if len(e.messages) > 0 {
			se.Messages = maps.Clone(e.messages)
		}
		out[tool] = se
	}
	return out
}

// restore folds a snapshot back into the store, the decode half of snapshot.
func (s *ApprovalStore) restore(entries map[string]serialApprovalEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tool, se := range entries {
		e := s.entryFor(tool)
		e.approvedAll = se.ApprovedAll
		e.rejectedAll = se.RejectedAll
		e.stickyMessage = se.StickyMessage
		for _, id := range se.ApprovedIDs {
			e.approvedIDs[id] = true
		}
		for _, id := range se.RejectedIDs {
			e.rejectedIDs[id] = true
		}
		maps.Copy(e.messages, se.Messages)
	}
}

// mirrorInto copies this store's decisions for each item into dst; "always"
// decisions are copied first so the call's own lands on top.
func (s *ApprovalStore) mirrorInto(dst *ApprovalStore, items []*ToolApprovalItem) {
	if dst == nil {
		return
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		s.mu.Lock()
		e := s.entries[it.ToolName]
		var apply []func()
		item := it
		if e != nil {
			switch {
			case e.approvedAll:
				apply = append(apply, func() { dst.Approve(item, true) })
			case e.rejectedAll:
				msg := e.stickyMessage
				apply = append(apply, func() { dst.Reject(item, true, msg) })
			}
			switch {
			case e.approvedIDs[it.CallID]:
				apply = append(apply, func() { dst.Approve(item, false) })
			case e.rejectedIDs[it.CallID]:
				msg := e.messages[it.CallID]
				apply = append(apply, func() { dst.Reject(item, false, msg) })
			}
		}
		s.mu.Unlock()
		for _, f := range apply {
			f()
		}
	}
}

// ToolContext is the RunContext plus metadata about the specific tool call.
type ToolContext struct {
	*RunContext
	// ToolName is the name of the tool being invoked.
	ToolName string
	// ToolCallID is the model-assigned identifier for this tool call.
	ToolCallID string
	// ToolArguments is the raw JSON arguments string emitted by the model.
	ToolArguments string
	// Agent is the agent whose tool is being invoked.
	Agent *Agent
	// ToolCall is the raw model-emitted function-call item that triggered this
	// invocation.
	ToolCall OutputItem
	// functionSpanID is this call's span id, the parent of a nested run's agent spans.
	functionSpanID string

	// emit pushes a partial result. Nil outside a streamed run.
	emit func(ToolResult)
	// done marks the call as finished, after which Emit is ignored.
	done atomic.Bool
}

// Emit pushes a partial result to a streamed run's consumer; ignored after the
// tool returns, a no-op on a blocking run, safe from any goroutine — see spec §2.7g.
func (tc *ToolContext) Emit(partial ToolResult) {
	if tc == nil || tc.emit == nil || tc.done.Load() {
		return
	}
	tc.emit(partial)
}

// streaming reports whether anyone is watching this call's progress.
func (tc *ToolContext) streaming() bool {
	return tc != nil && tc.emit != nil
}

// finish stops Emit from delivering anything further.
func (tc *ToolContext) finish() {
	if tc != nil {
		tc.done.Store(true)
	}
}
