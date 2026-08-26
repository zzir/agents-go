package agents

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/zzir/agents-go/tracing"
)

// RunContext carries user-supplied data and run-scoped state through a single
// agent run. It is passed to tool invocations, guardrails and lifecycle hooks.
//
// User data lives in the Context field as an any value; tool authors type-assert
// it back to their concrete type (see decisions §5.12). The standard library
// context.Context stays separate, for cancellation and deadlines.
type RunContext struct {
	// Context is the arbitrary user value threaded through the run. It is never
	// inspected by the SDK.
	Context any
	// Usage accumulates token usage across every model call in the run. It is
	// LIVE while the run executes — parallel agent-as-tool runs fold their
	// usage in concurrently — so mid-run readers (a budget check inside a
	// tool, a streaming consumer) must go through Usage.Snapshot rather than
	// the bare counter fields. Results hand out detached copies instead.
	Usage *Usage
	// Approvals tracks human-in-the-loop tool approval decisions.
	Approvals *ApprovalStore

	// turnInputMu guards turnInput: the run loop refreshes it while tools,
	// guardrails and hooks read it from their own goroutines.
	turnInputMu sync.RWMutex
	turnInput   []InputItem

	// inheritedOpts carries the run's model provider/model so nested runs (e.g.
	// agent-as-tool) inherit them. Set by the runner; not user-facing.
	inheritedOpts *RunOptions

	// activeTrace is the run's trace handle, so nested runs join it instead of
	// starting an orphan root trace. Set by the runner; not user-facing.
	activeTrace *tracing.TraceHandle

	// nestedToolStates caches the paused RunState of an agent-as-tool nested run,
	// keyed by parent tool call id, so a resumed parent continues it instead of
	// restarting. Guarded by nestedMu: a resume replays the turn's tool calls
	// concurrently, so two paused agent-tools take their states in parallel.
	nestedMu         sync.Mutex
	nestedToolStates map[string]*RunState
}

// TurnInput returns the model input for the turn currently executing: exactly
// what was sent to the model, after session history, handoff filtering,
// compaction and any CallModelInputFilter have been applied.
//
// Under UsePreviousResponseID or ConversationID the server holds the history
// and only new items are sent, so TurnInput reports those new items — what went
// on the wire, not a reconstruction of the full conversation.
//
// It is empty before the first turn's input is built. The returned slice is a
// copy, but the items in it are shared with the live request: treat them as
// read-only.
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

// setTurnInput publishes the turn's model input. The runner calls it once the
// input is final, and again if CallModelInputFilter edits it.
func (rc *RunContext) setTurnInput(items []InputItem) {
	if rc == nil {
		return
	}
	rc.turnInputMu.Lock()
	rc.turnInput = items
	rc.turnInputMu.Unlock()
}

// takeNestedToolState returns and removes the cached nested run state for a
// parent tool call id, if any. Used by an agent-as-tool on resume to continue
// its paused nested run.
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
// allow/deny sets. Precedence when resolving a call: a permanent approval wins over everything (including a
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
// tool name has no entry or the call is undecided. Precedence: permanent
// approval, permanent rejection, per-call approval, per-call rejection.
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

// snapshot lifts every recorded decision out of the store in its serialized
// form, for RunState.MarshalJSON. The call-id lists come out sorted so two
// otherwise identical runs serialize to identical bytes.
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

// mirrorInto copies this store's decision for each of the given approval items
// into dst, keyed by the item's call id. An agent-as-tool uses it to carry the
// parent run's approve/reject decisions into the nested run it resumes, so the
// human's choice on a surfaced nested interruption actually takes effect.
// Permanent (always) decisions stay permanent in the nested store — so an
// "always approve" on the parent covers the nested run's future calls to the
// same tool without another pause/resume round-trip.
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
		var apply func()
		item := it
		switch {
		case e == nil:
		case e.approvedAll:
			apply = func() { dst.Approve(item, true) }
		case e.rejectedAll:
			msg := e.stickyMessage
			apply = func() { dst.Reject(item, true, msg) }
		case e.approvedIDs[it.CallID]:
			apply = func() { dst.Approve(item, false) }
		case e.rejectedIDs[it.CallID]:
			msg := e.messages[it.CallID]
			apply = func() { dst.Reject(item, false, msg) }
		}
		s.mu.Unlock()
		if apply != nil {
			apply()
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
	// Agent is the agent whose tool is being invoked.
	Agent *Agent
	// ToolCall is the raw model-emitted function-call output item that triggered
	// this invocation.
	ToolCall OutputItem
	// functionSpanID is the tracing span ID of this tool call, letting a
	// nested agent-as-tool run parent its agent spans under the function span
	// instead of floating at the trace root.
	functionSpanID string

	// emit pushes a partial result. Nil outside a streamed run.
	emit func(ToolResult)
	// done marks the call as finished, after which Emit is ignored.
	done atomic.Bool
}

// Emit pushes a partial result to a streamed run's consumer.
//
// It is how a long tool call stays watchable — a command producing output for
// two minutes, a patch applying file by file — instead of showing a spinner
// until it is over.
//
// Scope is THIS call. After the tool returns, Emit is ignored: a goroutine the
// tool left running would otherwise keep pushing progress for a call that is
// already answered, and a consumer would have no way to tell that from a call
// still working.
//
// It is a no-op on a non-streamed run and safe to call from any goroutine, so
// a tool never needs to ask which kind of run it is in.
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
