package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"

	"github.com/openai/openai-go/v3/responses"
)

// RunStateSchemaVersion is the version stamped into serialized RunState. The
// format guarantees Go↔Go round-trips; it is not binary-compatible with the
// Python SDK's RunState.
//
// The version is checked for STRICT equality on decode: a state stamped with any
// other version — older or newer — is rejected rather than best-effort decoded
// (see RunStateFromJSON; recorded in docs/migration_from_python.md). The list below
// is why each version bumped, not a promise of cross-version compatibility: 1.1
// replaced the per-call approval maps with per-tool entries, 1.2 added the nested
// agent-as-tool states and the reasoning-item id policy, and 1.3 added the
// guardrail-result slices (input, output, tool input/output).
const RunStateSchemaVersion = "1.3"

// RunState is the serializable state of a run paused for human-in-the-loop tool
// approval. Obtain one from RunResult.State, record approvals/rejections via
// Approve/Reject, then continue with ResumeRun.
//
// It is the Go counterpart of the Python SDK's RunState. Serialize it with
// MarshalJSON to persist across processes and rebuild with RunStateFromJSON.
type RunState struct {
	CurrentAgent        *Agent
	OriginalInput       []TResponseInputItem
	GeneratedItems      []RunItem
	RawResponses        []*ModelResponse
	InterruptedResponse *ModelResponse
	Interruptions       []*ToolApprovalItem
	Approvals           *ApprovalStore
	Usage               *Usage
	CurrentTurn         int

	// MaxTurns is the turn budget of the interrupted run. ResumeRun always
	// continues under it (ignoring RunOptions.Exec.MaxTurns, matching Python), so a
	// run started with MaxTurns 20 and interrupted at turn 12 resumes under 20.
	// Zero — e.g. states serialized before this field existed — falls back to
	// DefaultMaxTurns; a negative value (MaxTurnsUnlimited) disables the budget.
	MaxTurns int

	// UserInput is the new input the interrupted Run was invoked with (without
	// session history), so the resumed run can persist it to the session.
	UserInput []TResponseInputItem

	// SessionItems is the full item log for session persistence; it differs from
	// GeneratedItems only when a handoff input filter rewrote the conversation.
	// Nil means it equals GeneratedItems.
	SessionItems []RunItem

	// PersistedSessionItems counts how many leading SessionItems the interrupted
	// run already wrote to the session before pausing (the pending, output-less
	// tool calls are held back). The resumed run continues persisting from here.
	// Zero — including states serialized before this field existed — makes the
	// resume re-persist from the start, which is safe: an old interrupted run
	// saved nothing, so there is nothing to double up.
	PersistedSessionItems int

	// ToolsUsed lists the names of agents that had already called tools when the
	// run paused, so ResumeRun keeps the tool_choice reset in effect for them
	// (Python serializes its tool-use tracker snapshot). Empty for states from
	// before this field existed — the interrupted agent re-marks itself when its
	// response is re-processed, so only cross-agent hand-back loses the reset.
	ToolsUsed []string

	// DisclosedTools names the deferred tools opened up before the pause, so a
	// resumed run does not re-hide a tool the model has already been told
	// about — which would look, from the model's side, like the tool was taken
	// away mid-conversation.
	DisclosedTools []string `json:"disclosed_tools,omitzero"`

	// PendingInput carries input queued through RunControl that the run had not
	// consumed when it paused. Without it, a steer sent while the caller was
	// deciding on an approval would be lost at exactly the moment it mattered
	// most — the human is looking at the run and saying something about it.
	PendingInput PendingInput `json:"pending_input,omitzero"`

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

	// nestedToolStates carries the paused RunState of any agent-as-tool nested
	// run, keyed by the parent tool call id, so ResumeRun continues the nested
	// run instead of restarting it. It rides on the live RunState across an
	// in-process pause/resume and is serialized RECURSIVELY in the RunState JSON
	// (each nested state round-trips through the same agent-registry rebuild), so
	// a cross-process resume continues the nested run too. Absent in states
	// serialized before this field existed → nil (a resumed nested run starts
	// fresh, the pre-1.2 behavior).
	nestedToolStates map[string]*RunState
}

// Approve records approval for a pending tool call. Pass always=true to approve
// every future call to the same tool.
func (s *RunState) Approve(item *ToolApprovalItem, always bool) {
	if s.Approvals == nil {
		s.Approvals = NewApprovalStore()
	}
	s.Approvals.Approve(item, always)
}

// Reject records rejection for a pending tool call. message, if non-empty, is
// sent back to the model in place of the tool output. Pass always=true to reject
// every future call to the same tool.
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
// Items the interrupted segment already emitted before pausing (the paused
// turn's message and tool-call items) are not re-emitted; the stream picks up
// with the side effects of the approval decisions and every later turn.
func ResumeRun(ctx context.Context, state *RunState, opts RunOptions) (RunStream, RunControl) {
	ctrl := newRunControl()
	return func(yield func(StreamEvent, error) bool) {
		resumeStream(ctx, state, opts, ctrl, true, yield)
	}, ctrl
}

// ResumeRunSync continues a paused run to completion and returns its result.
// It is ResumeRun without the stream, matching RunSync.
func ResumeRunSync(ctx context.Context, state *RunState, opts RunOptions) (*RunResult, error) {
	ctrl := newRunControl()
	stream := RunStream(func(yield func(StreamEvent, error) bool) {
		resumeStream(ctx, state, opts, ctrl, false, yield)
	})
	return stream.Collect()
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
		return nil, nil, newUserError("ResumeRun: state must not be nil")
	}
	if err := validateServerState(opts); err != nil {
		return nil, nil, err
	}
	// Turn budget on resume: the interrupted run's own budget always wins, so
	// repeated interrupt/resume cycles stay under the original limit
	// (Python parity: Runner.run ignores the max_turns argument when the input
	// is a RunState). A negative budget (MaxTurnsUnlimited) is preserved; only a
	// zero — including states serialized before MaxTurns existed — falls back to
	// the default.
	maxTurns := state.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}
	// Reasoning-item id policy precedence: an explicit override in opts wins, then
	// the interrupted run's own policy carried by the state (so a run started with
	// Omit keeps stripping ids on resume even when the caller does not repeat it).
	if opts.Exec.ReasoningItemIDPolicy == ReasoningItemIDPreserve {
		opts.Exec.ReasoningItemIDPolicy = state.ReasoningItemIDPolicy
	}
	// Input queued before the pause is delivered by this resume.
	ctrl.restore(state.PendingInput)

	rc := opts.RunContext
	if rc == nil {
		rc = NewRunContext(opts.Context)
	}
	if state.Approvals != nil {
		rc.Approvals = state.Approvals
	}
	if state.Usage != nil {
		rc.Usage = state.Usage
	}
	// Re-install any paused agent-as-tool nested states so a resumed AsTool call
	// continues its nested run. These survive a JSON round-trip (schema ≥ 1.2),
	// so a cross-process resume continues the nested run too.
	rc.nestedToolStates = state.nestedToolStates
	rc.inheritedOpts = &opts
	// Scrub the resumed input the same way a fresh run scrubs session history:
	// a state that was serialized, moved across processes, or hand-edited may
	// carry a dangling tool call the Responses API would reject. The pending
	// approval call lives in GeneratedItems (awaiting its output this turn), not
	// in OriginalInput, so it is untouched. The loop seeds originalInput from
	// r.resume.OriginalInput, so write the scrubbed form back onto the state
	// this ResumeRun is already consuming. Mirrors Python's
	// normalize_resumed_input.
	state.OriginalInput = normalizeStoredInput(state.OriginalInput)
	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, resume: state, userInput: state.UserInput, yield: yield, ctrl: ctrl, rawEvents: rawEvents}
	agentName := ""
	if state.CurrentAgent != nil {
		agentName = state.CurrentAgent.Name
	}
	r.log = newRunLogger(opts.Log).component("run").with(slog.String("agent", agentName), slog.Bool("resumed", true))
	r.diagnostics = &DiagnosticSink{}
	for _, name := range state.DisclosedTools {
		if r.disclosed == nil {
			r.disclosed = map[string]bool{}
		}
		r.disclosed[name] = true
	}
	// Seed the guardrail-result accumulators from the state so the resumed run's
	// RunResult still reports the pre-pause results. First-turn input guardrails
	// are not re-run on resume, so this is the only way they survive (Python
	// parity: the resume loop seeds its accumulators from run_state).
	r.guardrailResults = state.GuardrailResults
	// Restore the tool-use tracker so tool_choice stays reset for every agent
	// that had used tools before the pause (not only the interrupted one).
	if len(state.ToolsUsed) > 0 {
		r.toolsUsedBy = make(map[string]bool, len(state.ToolsUsed))
		for _, name := range state.ToolsUsed {
			r.toolsUsedBy[name] = true
		}
	}
	// A nested agent-as-tool resume passes the parent's live trace: join it
	// instead of opening an orphan "(resumed)" root trace (prepareRun parity).
	if opts.parentTrace != nil {
		r.trace = opts.parentTrace
	} else if opts.Observe.Tracer != nil {
		workflow := state.CurrentAgent.Name
		if workflow == "" {
			workflow = "Agent workflow"
		}
		r.trace = opts.Observe.Tracer.StartTrace(workflow + " (resumed)")
		defer r.trace.Finish()
	}
	rc.activeTrace = r.trace
	ctx = WithDiagnostics(ctx, r.diagnostics)
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
	// Source and Display carry the item's provenance and its UI projection
	// across an interruption. Without them a resumed run reports every restored
	// item as a plain model output with nothing to render, and a consumer that
	// reloads a paused conversation sees a different timeline than the one it
	// was showing before the pause.
	Source  Source      `json:"source,omitzero"`
	Display ItemDisplay `json:"display,omitzero"`
}

type serialResponse struct {
	ID     string            `json:"id"`
	Output []json.RawMessage `json:"output"`
	Usage  *Usage            `json:"usage,omitempty"`
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
	ModelResponses        []serialResponse               `json:"model_responses"`
	InterruptedResponse   *serialResponse                `json:"interrupted_response"`
	Interruptions         []serialInterruption           `json:"interruptions"`
	ApprovalEntries       map[string]serialApprovalEntry `json:"approval_entries,omitempty"`
	Usage                 *Usage                         `json:"usage,omitempty"`
	ReasoningItemIDPolicy string                         `json:"reasoning_item_id_policy,omitempty"`
	// NestedToolStates holds the serialized paused RunState of each agent-as-tool
	// nested run, keyed by the parent tool call id. Each value is a full RunState
	// JSON that round-trips through RunStateFromJSON with the same agent registry.
	NestedToolStates map[string]json.RawMessage `json:"nested_tool_states,omitempty"`

	// Guardrail results accumulated before the pause (schema ≥ 1.3), serialized
	// lossily as name + output payload — the live guardrail func is not restored.
	GuardrailResults []serialGuardrailResult `json:"guardrail_results,omitempty"`
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
// live Run func does not round-trip), mirroring Python's guardrail-result revival.

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
		Usage:                 s.Usage,
		ReasoningItemIDPolicy: reasoningPolicyToString(s.ReasoningItemIDPolicy),
		GuardrailResults:      toSerialGuardrailResults(s.GuardrailResults),
	}
	if s.CurrentAgent != nil {
		out.CurrentAgent = s.CurrentAgent.Name
	}
	// Serialize nested agent-as-tool states recursively: each nested *RunState
	// round-trips through its own MarshalJSON, so a cross-process resume rebuilds
	// and continues the nested run instead of restarting it.
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
	for i := range s.OriginalInput {
		raw, err := json.Marshal(s.OriginalInput[i])
		if err != nil {
			return nil, err
		}
		out.OriginalInput = append(out.OriginalInput, raw)
	}
	for i := range s.UserInput {
		raw, err := json.Marshal(s.UserInput[i])
		if err != nil {
			return nil, err
		}
		out.UserInput = append(out.UserInput, raw)
	}
	var err error
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
		out.ApprovalEntries = map[string]serialApprovalEntry{}
		s.Approvals.mu.Lock()
		for tool, e := range s.Approvals.entries {
			se := serialApprovalEntry{
				ApprovedAll:   e.approvedAll,
				RejectedAll:   e.rejectedAll,
				StickyMessage: e.stickyMessage,
			}
			for id := range e.approvedIDs {
				se.ApprovedIDs = append(se.ApprovedIDs, id)
			}
			for id := range e.rejectedIDs {
				se.RejectedIDs = append(se.RejectedIDs, id)
			}
			if len(e.messages) > 0 {
				se.Messages = maps.Clone(e.messages)
			}
			out.ApprovalEntries[tool] = se
		}
		s.Approvals.mu.Unlock()
	}
	return json.Marshal(out)
}

// serializeItems converts RunItems to their serialized input-item form.
func serializeItems(items []RunItem) ([]serialItem, error) {
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
		if a := it.AgentRef(); a != nil {
			agentName = a.Name
		}
		si := serialItem{
			Type:    it.ItemType(),
			Agent:   agentName,
			Input:   raw,
			Source:  it.Source(),
			Display: it.Display(),
		}
		out = append(out, si)
	}
	return out, nil
}

func serializeResponse(resp *ModelResponse) serialResponse {
	sr := serialResponse{ID: resp.ResponseID, Usage: resp.Usage}
	for i := range resp.Output {
		sr.Output = append(sr.Output, json.RawMessage(resp.Output[i].RawJSON()))
	}
	return sr
}

// RunStateFromJSON rebuilds a RunState from JSON produced by MarshalJSON. The
// registry maps agent names to *Agent so the runner can resolve the current
// agent and item agents; it must include every agent that participated in the
// run.
func RunStateFromJSON(data []byte, registry map[string]*Agent) (*RunState, error) {
	var in serialRunState
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("decoding run state: %w", err)
	}
	if in.SchemaVersion != RunStateSchemaVersion {
		return nil, newUserError("unsupported run state schema version %q (want %q)", in.SchemaVersion, RunStateSchemaVersion)
	}
	lookup := func(name string) *Agent { return registry[name] }

	st := &RunState{
		CurrentAgent:          lookup(in.CurrentAgent),
		CurrentTurn:           in.CurrentTurn,
		MaxTurns:              in.MaxTurns,
		PersistedSessionItems: in.PersistedSessionItems,
		ToolsUsed:             in.ToolsUsed,
		Usage:                 in.Usage,
		ReasoningItemIDPolicy: reasoningPolicyFromString(in.ReasoningItemIDPolicy),
		GuardrailResults:      fromSerialGuardrailResults(in.GuardrailResults),
		Approvals:             NewApprovalStore(),
	}
	if st.CurrentAgent == nil {
		return nil, newUserError("run state references unknown agent %q; add it to the registry", in.CurrentAgent)
	}
	if st.Usage == nil {
		st.Usage = NewUsage()
	}

	for _, raw := range in.OriginalInput {
		item, err := UnmarshalInputItem(raw)
		if err != nil {
			return nil, err
		}
		st.OriginalInput = append(st.OriginalInput, item)
	}
	for _, raw := range in.UserInput {
		item, err := UnmarshalInputItem(raw)
		if err != nil {
			return nil, err
		}
		st.UserInput = append(st.UserInput, item)
	}
	var err error
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
	// Preferred format (schema ≥ 1.1): per-tool entries.
	for tool, se := range in.ApprovalEntries {
		e := st.Approvals.entryFor(tool)
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
	// Rebuild nested agent-as-tool states recursively, resolving each nested
	// CurrentAgent via the same registry, so a resumed parent continues (not
	// restarts) its paused nested runs. Absent (pre-1.2 states) leaves the map
	// nil — a resumed nested run starts fresh.
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
func deserializeItems(items []serialItem, lookup func(string) *Agent) ([]RunItem, error) {
	var out []RunItem
	for _, si := range items {
		item, err := UnmarshalInputItem(si.Input)
		if err != nil {
			return nil, err
		}
		if si.Type == "tool_call_output" && len(si.Display.Extra) > 0 {
			// Restore as a typed item so the SDK-only extra data stays
			// reachable after a round-trip. Output is not serialized; only the
			// replayed input form and the extra data survive.
			out = append(out, &ToolCallOutputItem{
				Agent:   lookup(si.Agent),
				Raw:     item,
				Extra:   si.Display.Extra,
				IsError: si.Display.IsError,
			})
			continue
		}
		out = append(out, &rawInputRunItem{
			Agent:    lookup(si.Agent),
			RawInput: item,
			Kind:     si.Type,
			Src:      si.Source,
			Disp:     si.Display,
		})
	}
	return out, nil
}

func deserializeResponse(sr serialResponse) (*ModelResponse, error) {
	resp := &ModelResponse{ResponseID: sr.ID, Usage: sr.Usage}
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
