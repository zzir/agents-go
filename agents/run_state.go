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

// RunStateSchemaVersion is the version stamped into serialized RunState. It
// round-trips within this SDK only. Decoding accepts the same major, no newer
// than this minor and no older than runStateOldestDecodableMinor; the minors
// name format steps, not releases — see decisions §5.18.
const RunStateSchemaVersion = "1.6"

// runStateOldestDecodableMinor is the oldest minor this decoder accepts. Raise
// it when a bump REPLACES or reinterprets a field — decisions §5.18.
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

	// MaxTurns is the interrupted run's turn budget; ResumeRun continues under
	// it, ignoring RunOptions.Exec.MaxTurns. Zero → DefaultMaxTurns; negative
	// (MaxTurnsUnlimited) disables the budget.
	MaxTurns int

	// UserInput is the new input the interrupted Run was invoked with (without
	// session history), so the resumed run can persist it to the session.
	UserInput []InputItem

	// SessionItems is the run's full item log; GeneratedItems is its tail, the
	// items the model still sees (a resume takes it as such, by length). Nil
	// means the two are one.
	SessionItems []*RunItem

	// PersistedSessionItems counts the leading SessionItems already written
	// before the pause; the resume continues from here. Zero re-persists all.
	PersistedSessionItems int

	// ToolsUsed lists the agents that had called tools when the run paused, so
	// ResumeRun keeps their tool_choice reset in effect.
	ToolsUsed []string

	// OffChainHistory records that the stored log held items no model call
	// carried; the resume's only source (runner.offChainItems). Absent → false.
	OffChainHistory bool

	// DisclosedTools names the deferred tools opened up before the pause, so a
	// resumed run does not re-hide a tool the model has already been told about.
	DisclosedTools []string

	// PendingInput carries input queued through RunControl that the run had
	// not consumed when it paused — spec §2.11b.
	PendingInput PendingInput

	// ReasoningItemIDPolicy carries the interrupted run's policy so a resume
	// keeps it without the caller repeating the option. Absent → Preserve.
	ReasoningItemIDPolicy ReasoningItemIDPolicy

	// GuardrailResults carries every result accumulated before the pause, the
	// only source of first-turn input results on resume. Serialized lossily: a
	// decoded result carries a name-only stub guardrail.
	GuardrailResults []GuardrailResult

	// Extra is host-owned state riding the pause, carried verbatim and never
	// read by the SDK. It covers pause→resume, not crashes — decisions §5.18.
	// Absent → nil.
	Extra map[string]json.RawMessage

	// cursor is the server-managed-conversation cursor at the pause, so a
	// resume keeps sending deltas.
	cursor serverCursor

	// nestedToolStates carries paused agent-as-tool nested states, keyed by
	// parent tool call id, serialized recursively. Absent → nil.
	nestedToolStates map[string]*RunState

	// usagePending records whether the interrupted response's usage was still
	// unattributed at the pause; re-armed only then — spec §2.7f.
	usagePending bool
}

// Approve records approval for a pending tool call. Pass always=true to approve
// every future call to the same tool.
//
// Concurrent Approve/Reject calls are safe once Approvals is non-nil, which
// every state the SDK produces guarantees; a hand-constructed zero value must
// be seeded from one goroutine first.
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
// state, with Run's shape and semantics: nothing executes until the stream is
// ranged. Items emitted before the pause are not re-emitted. opts.Middlewares
// apply as in Run; a middleware may edit in.Opts, but the paused state's agent
// and input are already decided.
func ResumeRun(ctx context.Context, state *RunState, opts RunOptions) (RunStream, RunControl) {
	ctrl := newResumedControl(state)
	return resumeWithMiddleware(ctx, state, opts, ctrl, true), ctrl
}

// ResumeRunSync continues a paused run to completion and returns its result.
// It is ResumeRun without the stream, matching RunSync.
func ResumeRunSync(ctx context.Context, state *RunState, opts RunOptions) (*RunResult, error) {
	ctrl := newResumedControl(state)
	return resumeWithMiddleware(ctx, state, opts, ctrl, false).Collect()
}

// ResumeRunWith is ResumeRun under the control of the run that paused: the
// caller's StopAfterTurn and queued input keep working, and the control's live
// queue is carried as is rather than reseeded (spec §2.11b). ctrl must come
// from Run or ResumeRun; anything else panics.
func ResumeRunWith(ctx context.Context, state *RunState, opts RunOptions, ctrl RunControl) RunStream {
	c, ok := ctrl.(*runControl)
	if !ok {
		panic(fmt.Sprintf("agents: ResumeRunWith: the RunControl must come from Run or ResumeRun, got %T", ctrl))
	}
	return resumeWithMiddleware(ctx, state, opts, c, true)
}

// newResumedControl mints a control seeded from the paused state's queue
// before the caller can enqueue, so a new Steer sequences after the backlog.
func newResumedControl(state *RunState) *runControl {
	ctrl := newRunControl()
	if state != nil {
		ctrl.restore(state.PendingInput)
	}
	return ctrl
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
	base := func(ctx context.Context, in RunInput) RunStream {
		return func(yield func(StreamEvent, error) bool) {
			resumeStream(ctx, state, *in.Opts, ctrl, rawEvents, yield)
		}
	}
	return runViaMiddleware(ctx, state.CurrentAgent, state.OriginalInput, opts, ctrl, base)
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

// resumeLoop is the shared body of ResumeRun and ResumeRunSync. A nil runner
// means the failure predates one existing; the caller yields it directly.
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
		// A copy: a second resume of the same state (Retry over ResumeRun)
		// must start from the pause snapshot, not an inflated one.
		u := state.Usage.Snapshot()
		rc.Usage = &u
	}
	// A clone: taking a nested state deletes it, and a second resume must not
	// find the map depleted and restart each nested run.
	rc.nestedToolStates = maps.Clone(state.nestedToolStates)
	rc.inheritedOpts = &opts
	// Scrubbed like session history: a serialized or hand-edited state may
	// carry a dangling call. The pending approval call is in GeneratedItems.
	state.OriginalInput = normalizeStoredInput(state.OriginalInput)
	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, resume: state, userInput: state.UserInput, yield: yield, ctrl: ctrl, rawEvents: rawEvents}
	// The same start-up a fresh run gets: a nested resume joins the parent's
	// trace, a root one carries the caller's group id and metadata.
	finishTrace := r.observeRun(state.CurrentAgent, true)
	defer finishTrace()
	for _, name := range state.DisclosedTools {
		if r.disclosed == nil {
			r.disclosed = map[string]bool{}
		}
		r.disclosed[name] = true
	}
	// First-turn input guardrails are not re-run on resume; this is their only source.
	r.guardrailResults = state.GuardrailResults
	// Likewise off-chain history: a resume re-reads nothing and re-runs no filter.
	r.offChainHistory = state.OffChainHistory
	// Restore the tool-use tracker so tool_choice stays reset for every agent
	// that had used tools before the pause (not only the interrupted one).
	if len(state.ToolsUsed) > 0 {
		r.toolsUsedBy = make(map[string]bool, len(state.ToolsUsed))
		for _, name := range state.ToolsUsed {
			r.toolsUsedBy[name] = true
		}
	}
	res, err := r.execute(ctx, state.CurrentAgent, state.OriginalInput)
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
	// Status and IncompleteReason survive so Truncated() still reads true after
	// a cross-process resume — spec §2.7e.
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
	// NestedToolStates holds each agent-as-tool nested run's serialized paused
	// RunState, keyed by parent tool call id; decoded with the same registry.
	NestedToolStates map[string]json.RawMessage `json:"nested_tool_states,omitempty"`
	// UsagePending records an unattributed interrupted-response usage (see
	// RunState.usagePending); a pointer so absent decodes as re-arm.
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

// serialGuardrailResult is the persisted form of a guardrail result: name,
// stage and decision. The live Run func does not round-trip.
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

// marshalOutputInfo serializes a guardrail's OutputInfo, or nil when it is nil
// or unserializable — best-effort diagnostic data never fails the state.
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

// reasoningPolicyToString / reasoningPolicyFromString map the policy to its
// serialized form; Preserve (the default) serializes as absent.
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
// list, one raw message per item.
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
// by this build: same major, minor within the window.
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
	// Collects the names the registry misses (an empty name is "no agent") — see spec §2.5.
	var missingAgents []string
	seenMissing := map[string]bool{}
	lookup := func(name string) *Agent {
		a := registry[name]
		if a == nil && name != "" && !seenMissing[name] {
			seenMissing[name] = true
			missingAgents = append(missingAgents, name)
		}
		return a
	}

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
		// Absent means the debt exists: re-arming is the safe direction.
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
	if len(missingAgents) > 0 {
		return nil, NewUserError("run state names agents missing from the registry: %s", strings.Join(missingAgents, ", "))
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

// mergeNestedStates combines nested states still cached on the run context
// with those freshly paused this turn, preferring the fresh; nil when empty.
func mergeNestedStates(carried, fresh map[string]*RunState) map[string]*RunState {
	if len(carried) == 0 && len(fresh) == 0 {
		return nil
	}
	out := make(map[string]*RunState, len(carried)+len(fresh))
	maps.Copy(out, carried)
	maps.Copy(out, fresh)
	return out
}
