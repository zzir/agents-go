package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

// RunStateSchemaVersion is the version stamped into serialized RunState. The
// format guarantees round-trips within this SDK; it is not an interchange
// format with any other agents SDK.
//
// Decoding accepts the same major, no newer than this minor and no older than
// runStateOldestDecodableMinor; anything else is rejected rather than
// best-effort decoded (see RunStateFromJSON). The minors name format steps, not
// releases:
//
//	1.1 per-tool approval entries (replacing per-call maps)
//	1.2 nested agent-as-tool states + reasoning-item id policy
//	1.3 guardrail-result slices
//	1.4 pending injected input, disclosed deferred tools, server cursor
//	1.5 off-chain-history flag
//	1.6 host extra map
const RunStateSchemaVersion = "1.6"

// runStateOldestDecodableMinor is the oldest minor of the current major this
// decoder still accepts, so that a run paused before an SDK upgrade can resume
// after it: a state that old is missing only fields the decoder already falls
// back for.
//
// Raise it whenever a bump REPLACES or reinterprets a field rather than only
// adding one — such a state would decode with its old fields silently dropped,
// worse than a refusal. It sits at 4 because 1.3 was stamped both before and
// after the guardrail-result keys collapsed into one, a shape the version string
// cannot disambiguate; 1.5 and 1.6 only added fields, so a 1.4 state decodes.
const runStateOldestDecodableMinor = 4

// RunState is the serializable state of a run paused for human-in-the-loop tool
// approval. Obtain one from RunResult.State, record approvals/rejections via
// Approve/Reject, then continue with ResumeRun.
//
// Serialize it with MarshalJSON to persist across processes and rebuild with
// RunStateFromJSON.
type RunState struct {
	CurrentAgent        *Agent
	OriginalInput       []InputItem
	GeneratedItems      []*RunItem
	RawResponses        []*ModelResponse
	InterruptedResponse *ModelResponse
	Interruptions       []*ToolApprovalItem
	Approvals           *ApprovalStore
	// Usage is a detached copy of the usage accumulated up to the pause; a
	// resumed run adopts it and keeps adding, without touching the RunResult
	// the pause also returned.
	Usage       *Usage
	CurrentTurn int

	// MaxTurns is the turn budget of the interrupted run. ResumeRun always
	// continues under it, ignoring RunOptions.Exec.MaxTurns. Zero — including
	// states serialized before this field existed — falls back to DefaultMaxTurns;
	// a negative value (MaxTurnsUnlimited) disables the budget.
	MaxTurns int

	// UserInput is the new input the interrupted Run was invoked with (without
	// session history), so the resumed run can persist it to the session.
	UserInput []InputItem

	// SessionItems is the full item log for session persistence; it differs from
	// GeneratedItems only when a handoff input filter rewrote the conversation.
	// Nil means it equals GeneratedItems.
	SessionItems []*RunItem

	// PersistedSessionItems counts how many leading SessionItems the interrupted
	// run already wrote before pausing (pending output-less calls held back); the
	// resume continues from here. Zero — including pre-field states — re-persists
	// from the start, which is safe: an old interrupted run saved nothing.
	PersistedSessionItems int

	// ToolsUsed lists the agents that had already called tools when the run
	// paused, so ResumeRun keeps their tool_choice reset in effect. Empty for
	// pre-field states — the interrupted agent re-marks itself on re-process, so
	// only cross-agent hand-back loses the reset.
	ToolsUsed []string

	// OffChainHistory records that by the pause the stored log already held items
	// no model call carried — a read window truncated them, or a handoff filter
	// dropped them. The resume re-reads no history and re-runs no filter, so this
	// is its only source; getting it wrong lets a chain-based compaction delete
	// them unread (see runner.offChainItems). Absent (schema < 1.5) → false.
	OffChainHistory bool

	// DisclosedTools names the deferred tools opened up before the pause, so a
	// resumed run does not re-hide a tool the model has already been told about.
	DisclosedTools []string

	// PendingInput carries input queued through RunControl that the run had not
	// consumed when it paused — e.g. a steer sent while the caller was deciding
	// on an approval, which would otherwise be lost.
	PendingInput PendingInput

	// ReasoningItemIDPolicy carries the interrupted run's reasoning-item id
	// policy so a resumed run keeps stripping (or preserving) reasoning ids even
	// when the caller does not repeat the option. Absent in states serialized
	// before this field existed → ReasoningItemIDPreserve (the default).
	ReasoningItemIDPolicy ReasoningItemIDPolicy

	// GuardrailResults carries every guardrail result accumulated before the
	// pause, across all stages, so a resumed run's RunResult still reports them.
	// First-turn input guardrails are not re-run on resume, so the carried state
	// is their only source. Serialized lossily: the guardrail's live Run func
	// does not round-trip, so a decoded result carries a name-only stub.
	GuardrailResults []GuardrailResult

	// Extra is host-owned state riding the pause. The SDK carries it verbatim,
	// never reading a key, so a host can remember state across pause and resume.
	// Keys are the host's; a prefix ("plan:phase") avoids collisions. Absent in
	// states from before schema 1.6 → nil.
	//
	// It covers pause→resume, not crashes: a value lands here only when a pause
	// serializes the state. A fact that must survive a mid-run crash needs the
	// host's own durable write (PlanPhase.OnUnlock exists for that).
	Extra map[string]json.RawMessage

	// cursor is the server-managed-conversation cursor at the pause: what the
	// server already holds, so a resumed run keeps sending deltas rather than
	// re-sending the full history to a conversation that already has it.
	cursor serverCursor

	// nestedToolStates carries the paused RunState of any agent-as-tool nested
	// run, keyed by the parent tool call id, so ResumeRun continues the nested run
	// instead of restarting it. Serialized recursively, so a cross-process resume
	// continues it too. Absent (pre-1.2 states) → nil (nested run starts fresh).
	nestedToolStates map[string]*RunState

	// usagePending records whether the interrupted response's usage was still
	// unattributed when the run paused. The resumed runner re-arms attribution
	// only when this says the debt exists; unconditionally re-arming would
	// double-count a request the pausing segment had already attributed.
	usagePending bool
}

// Approve records approval for a pending tool call. Pass always=true to approve
// every future call to the same tool.
//
// Concurrent Approve/Reject calls are safe once Approvals is non-nil, which
// every state the SDK produces (a paused run, RunStateFromJSON) guarantees.
// The lazy init below only serves a hand-constructed zero value, and is not
// synchronized — such a state must be seeded from one goroutine first.
func (s *RunState) Approve(item *ToolApprovalItem, always bool) {
	if s.Approvals == nil {
		s.Approvals = NewApprovalStore()
	}
	s.Approvals.Approve(item, always)
}

// Reject records rejection for a pending tool call. message, if non-empty, is
// sent back to the model in place of the tool output. Pass always=true to reject
// every future call to the same tool. Concurrency: see Approve.
func (s *RunState) Reject(item *ToolApprovalItem, always bool, message string) {
	if s.Approvals == nil {
		s.Approvals = NewApprovalStore()
	}
	s.Approvals.Reject(item, always, message)
}

// ResumeRun continues a paused run after approvals have been recorded on the
// state, returning it as a stream plus a control handle — the same shape as
// Run, and with the same semantics: nothing executes until the stream is
// ranged.
//
// Items the interrupted segment already emitted before pausing are not
// re-emitted; the stream picks up with the side effects of the approval
// decisions and every later turn. ResumeRun applies opts.Middlewares exactly as
// Run does. A middleware may edit in.Opts, but the paused state's agent and
// input are already decided, so edits to those do not apply.
func ResumeRun(ctx context.Context, state *RunState, opts RunOptions) (RunStream, RunControl) {
	ctrl := newRunControl()
	return resumeWithMiddleware(ctx, state, opts, ctrl, true), ctrl
}

// ResumeRunSync continues a paused run to completion and returns its result.
// It is ResumeRun without the stream, matching RunSync.
func ResumeRunSync(ctx context.Context, state *RunState, opts RunOptions) (*RunResult, error) {
	ctrl := newRunControl()
	return resumeWithMiddleware(ctx, state, opts, ctrl, false).Collect()
}

// resumeWithMiddleware is ResumeRun's counterpart of withMiddleware.
func resumeWithMiddleware(ctx context.Context, state *RunState, opts RunOptions, ctrl *runControl, rawEvents bool) RunStream {
	if state == nil {
		// Let resumeStream report the nil-state error on the stream; reading
		// state fields below would panic before any middleware could see it.
		return singleUse(func(yield func(StreamEvent, error) bool) {
			resumeStream(ctx, nil, opts, ctrl, rawEvents, yield)
		})
	}
	// Seed the queue from the paused state before the control reaches the caller,
	// not lazily when ranging begins: a Steer enqueued in that window would
	// otherwise sequence ahead of the restored pre-pause backlog. (restore seeds
	// once per control.)
	ctrl.restore(state.PendingInput)
	base := func(ctx context.Context, in RunInput) RunStream {
		return func(yield func(StreamEvent, error) bool) {
			resumeStream(ctx, state, *in.Opts, ctrl, rawEvents, yield)
		}
	}
	return runViaMiddleware(ctx, state.CurrentAgent, state.OriginalInput, opts, base)
}

func resumeStream(ctx context.Context, state *RunState, opts RunOptions, ctrl *runControl, rawEvents bool, yield func(StreamEvent, error) bool) {
	r, res, err := resumeLoop(ctx, state, opts, ctrl, rawEvents, yield)
	if r == nil {
		// The failure happened before a runner existed (nil state, bad options).
		yield(nil, err)
		return
	}
	r.finishStream(res, err)
}

// resumeLoop is the shared body of ResumeRun and ResumeRunSync. It returns the
// runner alongside the outcome so the caller can report through the stream; a
// nil runner means the failure predates one existing.
func resumeLoop(ctx context.Context, state *RunState, opts RunOptions, ctrl *runControl, rawEvents bool, yield func(StreamEvent, error) bool) (*runner, *RunResult, error) {
	if state == nil {
		return nil, nil, NewUserError("ResumeRun: state must not be nil")
	}
	// Every state the SDK produces carries the agent it paused on; a
	// hand-constructed one may not, and the loop dereferences it throughout.
	if state.CurrentAgent == nil {
		return nil, nil, NewUserError("ResumeRun: state.CurrentAgent must not be nil")
	}
	if err := validateServerState(opts); err != nil {
		return nil, nil, err
	}
	// The interrupted run's own budget always wins (see RunState.MaxTurns); only
	// a zero falls back to the default.
	maxTurns := state.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}
	// Reasoning-item id policy: an explicit opts override wins, else the state's
	// own policy, so a run started with Omit keeps stripping ids on resume.
	if opts.Exec.ReasoningItemIDPolicy == ReasoningItemIDPreserve {
		opts.Exec.ReasoningItemIDPolicy = state.ReasoningItemIDPolicy
	}
	rc := NewRunContext(opts.Context)
	if state.Approvals != nil {
		rc.Approvals = state.Approvals
	}
	if state.Usage != nil {
		// A copy, not the state's own accumulator: the resumed run keeps adding to
		// rc.Usage, and a second resume of the same state (Retry over ResumeRun)
		// must start from the pause snapshot, not an inflated one.
		u := state.Usage.Snapshot()
		rc.Usage = &u
	}
	// Re-install any paused agent-as-tool nested states so a resumed AsTool call
	// continues its nested run. A copy, not the state's own map: taking a nested
	// state deletes it, and a second resume must not find the map depleted and
	// restart each nested run.
	rc.nestedToolStates = maps.Clone(state.nestedToolStates)
	rc.inheritedOpts = &opts
	// Scrub the resumed input as a fresh run scrubs session history: a serialized
	// or hand-edited state may carry a dangling tool call the Responses API would
	// reject. The pending approval call lives in GeneratedItems, not OriginalInput,
	// so it is untouched. Write the scrubbed form back for the loop to seed from.
	state.OriginalInput = normalizeStoredInput(state.OriginalInput)
	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, resume: state, userInput: state.UserInput, yield: yield, ctrl: ctrl, rawEvents: rawEvents}
	// Same start-up a fresh run gets, trace included: a nested resume joins the
	// parent's trace instead of opening an orphan root, and a root one carries
	// the caller's group id and metadata.
	finishTrace := r.observeRun(state.CurrentAgent, true)
	defer finishTrace()
	for _, name := range state.DisclosedTools {
		if r.disclosed == nil {
			r.disclosed = map[string]bool{}
		}
		r.disclosed[name] = true
	}
	// Seed the guardrail-result accumulators from the state so the resumed run's
	// RunResult still reports the pre-pause results. First-turn input guardrails
	// are not re-run on resume, so this is the only way they survive.
	r.guardrailResults = state.GuardrailResults
	// Likewise for what the paused half left off the response chain: a resume
	// makes no windowed read and applies no handoff filter, so the state is its
	// only source.
	r.offChainHistory = state.OffChainHistory
	// Restore the tool-use tracker so tool_choice stays reset for every agent
	// that had used tools before the pause (not only the interrupted one).
	if len(state.ToolsUsed) > 0 {
		r.toolsUsedBy = make(map[string]bool, len(state.ToolsUsed))
		for _, name := range state.ToolsUsed {
			r.toolsUsedBy[name] = true
		}
	}
	ctx = WithDiagnostics(ctx, r.diagnostics)
	// Same cancellation root a fresh run installs (see runStream): emit
	// cancels it when the consumer stops ranging mid-resume.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.cancelRun = cancel
	res, err := r.loop(ctx, state.CurrentAgent, state.OriginalInput)
	if err == nil && res != nil && res.State != nil {
		// The resumed run interrupted again: carry the effective budget on the
		// new state so repeated interrupt/resume cycles keep it.
		res.State.MaxTurns = maxTurns
	}
	return r, res, err
}

// --- Serialization ---

type serialItem struct {
	Type  string          `json:"type"`
	Agent string          `json:"agent"`
	Input json.RawMessage `json:"input"`
	// Source and Display carry the item's provenance and UI projection across an
	// interruption, so a reloaded paused conversation renders the same timeline.
	Source  Source      `json:"source,omitzero"`
	Display ItemDisplay `json:"display,omitzero"`
}

type serialResponse struct {
	ID     string            `json:"id"`
	Output []json.RawMessage `json:"output"`
	Usage  *Usage            `json:"usage,omitempty"`
	// Status and IncompleteReason survive serialization because Truncated() reads
	// them: a response cut off at the output-token limit must still read as cut
	// off after a cross-process resume, or the resume runs tool calls with
	// mid-JSON arguments (spec §2.7e).
	Status           string `json:"status,omitempty"`
	IncompleteReason string `json:"incomplete_reason,omitempty"`
}

// serialApprovalEntry is the per-tool approval entry (schema ≥ 1.1): a permanent
// allow/deny plus per-call id sets and messages.
type serialApprovalEntry struct {
	ApprovedAll   bool              `json:"approved_all,omitempty"`
	RejectedAll   bool              `json:"rejected_all,omitempty"`
	ApprovedIDs   []string          `json:"approved_ids,omitempty"`
	RejectedIDs   []string          `json:"rejected_ids,omitempty"`
	Messages      map[string]string `json:"messages,omitempty"`
	StickyMessage string            `json:"sticky_message,omitempty"`
}

type serialInterruption struct {
	Agent     string          `json:"agent"`
	ToolName  string          `json:"tool_name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Raw       json.RawMessage `json:"raw"`
}

type serialRunState struct {
	SchemaVersion         string                         `json:"schema_version"`
	CurrentAgent          string                         `json:"current_agent"`
	CurrentTurn           int                            `json:"current_turn"`
	MaxTurns              int                            `json:"max_turns,omitempty"`
	OriginalInput         []json.RawMessage              `json:"original_input"`
	UserInput             []json.RawMessage              `json:"user_input,omitempty"`
	GeneratedItems        []serialItem                   `json:"generated_items"`
	SessionItems          []serialItem                   `json:"session_items,omitempty"`
	PersistedSessionItems int                            `json:"persisted_session_items,omitempty"`
	ToolsUsed             []string                       `json:"tools_used,omitempty"`
	OffChainHistory       bool                           `json:"off_chain_history,omitempty"`
	DisclosedTools        []string                       `json:"disclosed_tools,omitempty"`
	PendingInput          *serialPendingInput            `json:"pending_input,omitempty"`
	ServerCursor          *serialServerCursor            `json:"server_cursor,omitempty"`
	ModelResponses        []serialResponse               `json:"model_responses"`
	InterruptedResponse   *serialResponse                `json:"interrupted_response"`
	Interruptions         []serialInterruption           `json:"interruptions"`
	ApprovalEntries       map[string]serialApprovalEntry `json:"approval_entries,omitempty"`
	Usage                 *Usage                         `json:"usage,omitempty"`
	ReasoningItemIDPolicy string                         `json:"reasoning_item_id_policy,omitempty"`
	Extra                 map[string]json.RawMessage     `json:"extra,omitempty"`
	// NestedToolStates holds the serialized paused RunState of each agent-as-tool
	// nested run, keyed by the parent tool call id. Each value is a full RunState
	// JSON that round-trips through RunStateFromJSON with the same agent registry.
	NestedToolStates map[string]json.RawMessage `json:"nested_tool_states,omitempty"`
	// UsagePending records an unattributed interrupted-response usage; see
	// RunState.usagePending. A pointer so a pre-field nil decodes to the old
	// always-re-arm behavior.
	UsagePending *bool `json:"usage_pending,omitempty"`

	// Guardrail results accumulated before the pause (schema ≥ 1.3), serialized
	// lossily as name + output payload — the live guardrail func is not restored.
	GuardrailResults []serialGuardrailResult `json:"guardrail_results,omitempty"`
}

// serialPendingInput is the persisted form of RunState.PendingInput: one item
// list per RunControl queue.
type serialPendingInput struct {
	Steer    []json.RawMessage `json:"steer,omitempty"`
	NextTurn []json.RawMessage `json:"next_turn,omitempty"`
	FollowUp []json.RawMessage `json:"follow_up,omitempty"`
}

// serialServerCursor is the persisted form of the run's server-conversation
// cursor (serverCursor).
type serialServerCursor struct {
	ResponseID         string `json:"response_id,omitempty"`
	ItemCount          int    `json:"item_count,omitempty"`
	ConversationActive bool   `json:"conversation_active,omitempty"`
}

// serialGuardrailResult is the persisted form of a guardrail result at any
// stage: the guardrail's name and stage plus its decision. The guardrail's live
// Run func cannot serialize, so a decoded result carries a name-only stub.
type serialGuardrailResult struct {
	Name       string          `json:"name,omitempty"`
	Stage      string          `json:"stage,omitempty"`
	Action     int             `json:"action,omitempty"`
	Message    string          `json:"message,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Arguments  string          `json:"arguments,omitempty"`
	OutputInfo json.RawMessage `json:"output_info,omitempty"`
}

// marshalOutputInfo serializes a guardrail's OutputInfo (an arbitrary value) to
// raw JSON, or nil when it is nil or unserializable — the payload is best-effort
// diagnostic data, so a marshal failure drops it rather than failing the state.
func marshalOutputInfo(info any) json.RawMessage {
	if info == nil {
		return nil
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return nil
	}
	return raw
}

// unmarshalOutputInfo decodes a raw OutputInfo payload back into a generic
// value; nil raw (absent) stays nil.
func unmarshalOutputInfo(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

func toSerialGuardrailResults(rs []GuardrailResult) []serialGuardrailResult {
	if len(rs) == 0 {
		return nil
	}
	out := make([]serialGuardrailResult, len(rs))
	for i, r := range rs {
		out[i] = serialGuardrailResult{
			Name:       r.Guardrail.Name,
			Stage:      string(r.Stage),
			Action:     int(r.Decision.Action),
			Message:    r.Decision.Message,
			ToolName:   r.ToolName,
			ToolCallID: r.ToolCallID,
			Arguments:  r.Arguments,
			OutputInfo: marshalOutputInfo(r.Decision.OutputInfo),
		}
	}
	return out
}

// The from-serial helpers rebuild results with a name-only stub guardrail (the
// live Run func does not round-trip).

func fromSerialGuardrailResults(rs []serialGuardrailResult) []GuardrailResult {
	if len(rs) == 0 {
		return nil
	}
	out := make([]GuardrailResult, len(rs))
	for i, r := range rs {
		stage := GuardrailStage(r.Stage)
		out[i] = GuardrailResult{
			Guardrail:  Guardrail{Name: r.Name, Stages: []GuardrailStage{stage}},
			Stage:      stage,
			Decision:   GuardrailDecision{Action: GuardrailAction(r.Action), Message: r.Message, OutputInfo: unmarshalOutputInfo(r.OutputInfo)},
			ToolName:   r.ToolName,
			ToolCallID: r.ToolCallID,
			Arguments:  r.Arguments,
		}
	}
	return out
}

// reasoningPolicyToString / reasoningPolicyFromString map the typed policy to its
// serialized form. Preserve (the default) serializes as absent so old readers and
// old states round-trip unchanged.
func reasoningPolicyToString(p ReasoningItemIDPolicy) string {
	if p == ReasoningItemIDOmit {
		return "omit"
	}
	return ""
}

func reasoningPolicyFromString(s string) ReasoningItemIDPolicy {
	if s == "omit" {
		return ReasoningItemIDOmit
	}
	return ReasoningItemIDPreserve
}

// MarshalJSON serializes the run state to JSON for persistence.
func (s *RunState) MarshalJSON() ([]byte, error) {
	out := serialRunState{
		SchemaVersion:         RunStateSchemaVersion,
		CurrentTurn:           s.CurrentTurn,
		MaxTurns:              s.MaxTurns,
		PersistedSessionItems: s.PersistedSessionItems,
		ToolsUsed:             s.ToolsUsed,
		OffChainHistory:       s.OffChainHistory,
		DisclosedTools:        s.DisclosedTools,
		Usage:                 s.Usage,
		ReasoningItemIDPolicy: reasoningPolicyToString(s.ReasoningItemIDPolicy),
		GuardrailResults:      toSerialGuardrailResults(s.GuardrailResults),
		Extra:                 s.Extra,
		UsagePending:          &s.usagePending,
	}
	if s.CurrentAgent != nil {
		out.CurrentAgent = s.CurrentAgent.Name
	}
	if s.cursor != (serverCursor{}) {
		out.ServerCursor = &serialServerCursor{
			ResponseID:         s.cursor.responseID,
			ItemCount:          s.cursor.itemCount,
			ConversationActive: s.cursor.conversationActive,
		}
	}
	// Serialize nested agent-as-tool states recursively so a cross-process resume
	// can continue them.
	if len(s.nestedToolStates) > 0 {
		out.NestedToolStates = make(map[string]json.RawMessage, len(s.nestedToolStates))
		for callID, nested := range s.nestedToolStates {
			raw, err := json.Marshal(nested)
			if err != nil {
				return nil, err
			}
			out.NestedToolStates[callID] = raw
		}
	}
	var err error
	if out.OriginalInput, err = marshalInputItems(s.OriginalInput); err != nil {
		return nil, err
	}
	if out.UserInput, err = marshalInputItems(s.UserInput); err != nil {
		return nil, err
	}
	if !s.PendingInput.Empty() {
		p := &serialPendingInput{}
		if p.Steer, err = marshalInputItems(s.PendingInput.Steer); err != nil {
			return nil, err
		}
		if p.NextTurn, err = marshalInputItems(s.PendingInput.NextTurn); err != nil {
			return nil, err
		}
		if p.FollowUp, err = marshalInputItems(s.PendingInput.FollowUp); err != nil {
			return nil, err
		}
		out.PendingInput = p
	}
	if out.GeneratedItems, err = serializeItems(s.GeneratedItems); err != nil {
		return nil, err
	}
	if out.SessionItems, err = serializeItems(s.SessionItems); err != nil {
		return nil, err
	}
	for _, resp := range s.RawResponses {
		out.ModelResponses = append(out.ModelResponses, serializeResponse(resp))
	}
	if s.InterruptedResponse != nil {
		ir := serializeResponse(s.InterruptedResponse)
		out.InterruptedResponse = &ir
	}
	for _, it := range s.Interruptions {
		raw := json.RawMessage(it.Raw.RawJSON())
		agentName := ""
		if it.Agent != nil {
			agentName = it.Agent.Name
		}
		out.Interruptions = append(out.Interruptions, serialInterruption{
			Agent: agentName, ToolName: it.ToolName, CallID: it.CallID, Arguments: it.Arguments, Raw: raw,
		})
	}
	if s.Approvals != nil {
		out.ApprovalEntries = s.Approvals.snapshot()
	}
	return json.Marshal(out)
}

// marshalInputItems and unmarshalInputItems round-trip a Responses input-item
// list, one raw message per item. Every item list on the state (original
// input, user input, the pending-input queues) serializes through them.
func marshalInputItems(items []InputItem) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for i := range items {
		raw, err := json.Marshal(items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func unmarshalInputItems(raw []json.RawMessage) ([]InputItem, error) {
	var out []InputItem
	for _, r := range raw {
		item, err := session.UnmarshalInputItem(r)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// serializeItems converts RunItems to their serialized input-item form.
func serializeItems(items []*RunItem) ([]serialItem, error) {
	var out []serialItem
	for _, it := range items {
		in, err := it.ToInputItem()
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		agentName := ""
		if it.Agent != nil {
			agentName = it.Agent.Name
		}
		si := serialItem{
			Type:    string(it.Kind),
			Agent:   agentName,
			Input:   raw,
			Source:  it.Source,
			Display: it.Display(),
		}
		out = append(out, si)
	}
	return out, nil
}

func serializeResponse(resp *ModelResponse) serialResponse {
	sr := serialResponse{
		ID:               resp.ResponseID,
		Usage:            resp.Usage,
		Status:           resp.Status,
		IncompleteReason: resp.IncompleteReason,
	}
	for i := range resp.Output {
		sr.Output = append(sr.Output, json.RawMessage(resp.Output[i].RawJSON()))
	}
	return sr
}

// RunStateVersionSupported reports whether a serialized state stamped with
// version can be decoded by this SDK: same major, minor no newer than
// RunStateSchemaVersion and no older than the oldest this decoder accepts. It
// answers from the version string alone, so a host can triage a stored state
// without a registry or a full decode.
func RunStateVersionSupported(version string) bool {
	return checkRunStateSchemaVersion(version) == nil
}

// checkRunStateSchemaVersion decides whether a serialized state can be decoded
// by this build: same major, no newer than RunStateSchemaVersion, no older than
// runStateOldestDecodableMinor.
func checkRunStateSchemaVersion(v string) error {
	major, minor, ok := parseSchemaVersion(v)
	if !ok {
		return NewUserError("malformed run state schema version %q (want major.minor, e.g. %q)", v, RunStateSchemaVersion)
	}
	// The literal this package stamps, so it parses.
	wantMajor, wantMinor, _ := parseSchemaVersion(RunStateSchemaVersion)
	switch {
	case major != wantMajor:
		return NewUserError("run state schema version %q is from a different major version than this SDK's %q and cannot be decoded", v, RunStateSchemaVersion)
	case minor > wantMinor:
		return NewUserError("run state schema version %q is newer than this SDK's %q; resume it with the SDK that wrote it, or upgrade", v, RunStateSchemaVersion)
	case minor < runStateOldestDecodableMinor:
		return NewUserError("run state schema version %q is older than the oldest this SDK decodes (%d.%d)", v, wantMajor, runStateOldestDecodableMinor)
	}
	return nil
}

// parseSchemaVersion splits a "major.minor" version. Anything else — an absent
// version, a three-part one, a non-number — is not ok.
func parseSchemaVersion(v string) (major, minor int, ok bool) {
	majorText, minorText, found := strings.Cut(v, ".")
	if !found {
		return 0, 0, false
	}
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(minorText)
	if err != nil || minor < 0 {
		return 0, 0, false
	}
	return major, minor, true
}

// RunStateFromJSON rebuilds a RunState from JSON produced by MarshalJSON, by
// this SDK or by an earlier one whose schema minor this build still decodes
// (see RunStateSchemaVersion). The registry maps agent names to *Agent so the
// runner can resolve the current agent and item agents; it must include every
// agent that participated in the run.
func RunStateFromJSON(data []byte, registry map[string]*Agent) (*RunState, error) {
	var in serialRunState
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("decoding run state: %w", err)
	}
	if err := checkRunStateSchemaVersion(in.SchemaVersion); err != nil {
		return nil, err
	}
	lookup := func(name string) *Agent { return registry[name] }

	st := &RunState{
		CurrentAgent:          lookup(in.CurrentAgent),
		CurrentTurn:           in.CurrentTurn,
		MaxTurns:              in.MaxTurns,
		PersistedSessionItems: in.PersistedSessionItems,
		ToolsUsed:             in.ToolsUsed,
		OffChainHistory:       in.OffChainHistory,
		DisclosedTools:        in.DisclosedTools,
		Usage:                 in.Usage,
		ReasoningItemIDPolicy: reasoningPolicyFromString(in.ReasoningItemIDPolicy),
		GuardrailResults:      fromSerialGuardrailResults(in.GuardrailResults),
		Extra:                 in.Extra,
		Approvals:             NewApprovalStore(),
		// Absent (a pre-flag state) resumes with the old always-re-arm behavior.
		usagePending: in.UsagePending == nil || *in.UsagePending,
	}
	if st.CurrentAgent == nil {
		return nil, NewUserError("run state references unknown agent %q; add it to the registry", in.CurrentAgent)
	}
	if st.Usage == nil {
		st.Usage = NewUsage()
	}
	if in.ServerCursor != nil {
		st.cursor = serverCursor{
			responseID:         in.ServerCursor.ResponseID,
			itemCount:          in.ServerCursor.ItemCount,
			conversationActive: in.ServerCursor.ConversationActive,
		}
	}

	var err error
	if st.OriginalInput, err = unmarshalInputItems(in.OriginalInput); err != nil {
		return nil, err
	}
	if st.UserInput, err = unmarshalInputItems(in.UserInput); err != nil {
		return nil, err
	}
	if in.PendingInput != nil {
		if st.PendingInput.Steer, err = unmarshalInputItems(in.PendingInput.Steer); err != nil {
			return nil, err
		}
		if st.PendingInput.NextTurn, err = unmarshalInputItems(in.PendingInput.NextTurn); err != nil {
			return nil, err
		}
		if st.PendingInput.FollowUp, err = unmarshalInputItems(in.PendingInput.FollowUp); err != nil {
			return nil, err
		}
	}
	if st.GeneratedItems, err = deserializeItems(in.GeneratedItems, lookup); err != nil {
		return nil, err
	}
	if st.SessionItems, err = deserializeItems(in.SessionItems, lookup); err != nil {
		return nil, err
	}
	for _, sr := range in.ModelResponses {
		resp, err := deserializeResponse(sr)
		if err != nil {
			return nil, err
		}
		st.RawResponses = append(st.RawResponses, resp)
	}
	if in.InterruptedResponse != nil {
		resp, err := deserializeResponse(*in.InterruptedResponse)
		if err != nil {
			return nil, err
		}
		st.InterruptedResponse = resp
	}
	for _, si := range in.Interruptions {
		var raw responses.ResponseOutputItemUnion
		if err := json.Unmarshal(si.Raw, &raw); err != nil {
			return nil, err
		}
		st.Interruptions = append(st.Interruptions, &ToolApprovalItem{
			Agent: lookup(si.Agent), ToolName: si.ToolName, CallID: si.CallID, Arguments: si.Arguments, Raw: raw,
		})
	}
	st.Approvals.restore(in.ApprovalEntries)
	// Rebuild nested agent-as-tool states recursively via the same registry, so a
	// resumed parent continues (not restarts) them. Absent (pre-1.2) → nil.
	if len(in.NestedToolStates) > 0 {
		st.nestedToolStates = make(map[string]*RunState, len(in.NestedToolStates))
		for callID, raw := range in.NestedToolStates {
			nested, err := RunStateFromJSON(raw, registry)
			if err != nil {
				return nil, fmt.Errorf("decoding nested tool state %q: %w", callID, err)
			}
			st.nestedToolStates[callID] = nested
		}
	}
	return st, nil
}

// deserializeItems rebuilds RunItems from their serialized input-item form.
func deserializeItems(items []serialItem, lookup func(string) *Agent) ([]*RunItem, error) {
	var out []*RunItem
	for _, si := range items {
		item, err := session.UnmarshalInputItem(si.Input)
		if err != nil {
			return nil, err
		}
		disp := si.Display
		out = append(out, &RunItem{
			Kind:     ItemKind(si.Type),
			Agent:    lookup(si.Agent),
			Source:   si.Source,
			RawInput: &item,
			// SDK-only tool-result data survives the round-trip on the item
			// itself; Output is not serialized — only the replayed input form.
			Extra:   disp.Extra,
			IsError: disp.IsError,
			display: &disp,
		})
	}
	return out, nil
}

func deserializeResponse(sr serialResponse) (*ModelResponse, error) {
	resp := &ModelResponse{
		ResponseID:       sr.ID,
		Usage:            sr.Usage,
		Status:           sr.Status,
		IncompleteReason: sr.IncompleteReason,
	}
	if resp.Usage == nil {
		resp.Usage = NewUsage()
	}
	for _, raw := range sr.Output {
		var item responses.ResponseOutputItemUnion
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		resp.Output = append(resp.Output, item)
	}
	return resp, nil
}

// mergeNestedStates combines any agent-as-tool nested states still cached on
// the run context (un-consumed from a prior resume) with those freshly paused
// this turn, preferring the fresh ones. Returns nil when both are empty so a
// run without nested-tool HITL carries no map.
func mergeNestedStates(carried, fresh map[string]*RunState) map[string]*RunState {
	if len(carried) == 0 && len(fresh) == 0 {
		return nil
	}
	out := make(map[string]*RunState, len(carried)+len(fresh))
	maps.Copy(out, carried)
	maps.Copy(out, fresh)
	return out
}
