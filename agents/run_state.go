package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3/responses"
)

// RunStateSchemaVersion is the version stamped into serialized RunState. The
// format guarantees Go↔Go round-trips; it is not binary-compatible with the
// Python SDK's RunState.
const RunStateSchemaVersion = "1.0"

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

	// MaxTurns is the turn budget of the interrupted run, so ResumeRun can
	// continue under the same budget (a run started with MaxTurns 20 and
	// interrupted at turn 12 would otherwise be unresumable past the default).
	// Zero — e.g. states serialized before this field existed — falls back to
	// DefaultMaxTurns unless RunOptions.MaxTurns overrides it.
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
// state. It re-processes the interrupted model response (now that decisions are
// available) and runs the loop to completion or the next interruption.
func ResumeRun(ctx context.Context, state *RunState, opts RunOptions) (*RunResult, error) {
	return resumeLoop(ctx, state, opts, nil)
}

// ResumeRunStreamed is ResumeRun in streaming mode: it continues a paused run
// and streams events as they are produced, exactly like RunStreamed does for a
// fresh run. Items the interrupted segment already emitted before pausing (the
// paused turn's message and tool-call items) are not re-emitted; the stream
// picks up with the side effects of the approval decisions (tool outputs) and
// every later turn.
func ResumeRunStreamed(ctx context.Context, state *RunState, opts RunOptions) *StreamedResult {
	sr := &StreamedResult{ch: make(chan streamMsg, 64)}

	go func() {
		defer close(sr.ch)
		res, err := resumeLoop(ctx, state, opts, sr)
		sr.setFinal(res, err)
		if err != nil {
			// Same rationale as RunStreamed: the error is recorded via setFinal,
			// so when the consumer has gone away and the buffer is full, dropping
			// this send avoids leaking the goroutine.
			select {
			case sr.ch <- streamMsg{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	return sr
}

// resumeLoop is the shared body of ResumeRun and ResumeRunStreamed; a non-nil
// sr switches the loop into streaming mode.
func resumeLoop(ctx context.Context, state *RunState, opts RunOptions, sr *StreamedResult) (*RunResult, error) {
	if state == nil {
		return nil, newUserError("ResumeRun: state must not be nil")
	}
	if err := validateServerState(opts); err != nil {
		return nil, err
	}
	// Turn budget precedence: an explicit override in opts wins, then the
	// interrupted run's own budget carried by the state, then the default
	// (which is also the fallback for states serialized before MaxTurns
	// existed — those round-trip as zero).
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = state.MaxTurns
	}
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
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
	rc.inheritedOpts = &opts
	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, resume: state, userInput: state.UserInput, sr: sr}
	if opts.Tracer != nil {
		workflow := state.CurrentAgent.Name
		if workflow == "" {
			workflow = "Agent workflow"
		}
		r.trace = opts.Tracer.StartTrace(workflow + " (resumed)")
		defer r.trace.Finish()
	}
	rc.activeTrace = r.trace
	res, err := r.loop(ctx, state.CurrentAgent, state.OriginalInput)
	if err == nil && res != nil && res.State != nil {
		// The resumed run interrupted again: carry the effective budget on the
		// new state so repeated interrupt/resume cycles keep it.
		res.State.MaxTurns = maxTurns
	}
	return res, err
}

// --- Serialization ---

type serialItem struct {
	Type  string          `json:"type"`
	Agent string          `json:"agent"`
	Input json.RawMessage `json:"input"`
	// CustomData carries a ToolCallOutputItem's SDK-only custom data, so it
	// survives HITL interruptions. Absent for other item kinds.
	CustomData map[string]any `json:"custom_data,omitempty"`
}

type serialResponse struct {
	ID     string            `json:"id"`
	Output []json.RawMessage `json:"output"`
	Usage  *Usage            `json:"usage,omitempty"`
}

type serialApproval struct {
	Approved bool   `json:"approved"`
	Rejected bool   `json:"rejected"`
	Message  string `json:"message,omitempty"`
}

type serialInterruption struct {
	Agent     string          `json:"agent"`
	ToolName  string          `json:"tool_name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Raw       json.RawMessage `json:"raw"`
}

type serialRunState struct {
	SchemaVersion         string                    `json:"schema_version"`
	CurrentAgent          string                    `json:"current_agent"`
	CurrentTurn           int                       `json:"current_turn"`
	MaxTurns              int                       `json:"max_turns,omitempty"`
	OriginalInput         []json.RawMessage         `json:"original_input"`
	UserInput             []json.RawMessage         `json:"user_input,omitempty"`
	GeneratedItems        []serialItem              `json:"generated_items"`
	SessionItems          []serialItem              `json:"session_items,omitempty"`
	PersistedSessionItems int                       `json:"persisted_session_items,omitempty"`
	ModelResponses        []serialResponse          `json:"model_responses"`
	InterruptedResponse   *serialResponse           `json:"interrupted_response"`
	Interruptions         []serialInterruption      `json:"interruptions"`
	Approvals             map[string]serialApproval `json:"approvals_by_call_id"`
	ApprovalsByTool       map[string]serialApproval `json:"approvals_by_tool"`
	Usage                 *Usage                    `json:"usage,omitempty"`
}

// MarshalJSON serializes the run state to JSON for persistence.
func (s *RunState) MarshalJSON() ([]byte, error) {
	out := serialRunState{
		SchemaVersion:         RunStateSchemaVersion,
		CurrentTurn:           s.CurrentTurn,
		MaxTurns:              s.MaxTurns,
		PersistedSessionItems: s.PersistedSessionItems,
		Usage:                 s.Usage,
	}
	if s.CurrentAgent != nil {
		out.CurrentAgent = s.CurrentAgent.Name
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
		out.Approvals = map[string]serialApproval{}
		out.ApprovalsByTool = map[string]serialApproval{}
		s.Approvals.mu.Lock()
		for k, d := range s.Approvals.byCallID {
			out.Approvals[k] = serialApproval{Approved: d.approved, Rejected: d.rejected, Message: d.message}
		}
		for k, d := range s.Approvals.byToolName {
			out.ApprovalsByTool[k] = serialApproval{Approved: d.approved, Rejected: d.rejected, Message: d.message}
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
		si := serialItem{Type: it.ItemType(), Agent: agentName, Input: raw}
		if o, ok := it.(*ToolCallOutputItem); ok && len(o.CustomData) > 0 {
			si.CustomData = o.CustomData
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
		Usage:                 in.Usage,
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
	for k, d := range in.Approvals {
		st.Approvals.byCallID[k] = approvalDecision{approved: d.Approved, rejected: d.Rejected, message: d.Message}
	}
	for k, d := range in.ApprovalsByTool {
		st.Approvals.byToolName[k] = approvalDecision{approved: d.Approved, rejected: d.Rejected, message: d.Message}
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
		if len(si.CustomData) > 0 && si.Type == "tool_call_output" {
			// Restore as a typed item so the SDK-only custom data stays
			// reachable after a round-trip. Output is not serialized; only the
			// replayed input form and the custom data survive.
			out = append(out, &ToolCallOutputItem{Agent: lookup(si.Agent), Raw: item, CustomData: si.CustomData})
			continue
		}
		out = append(out, &rawInputRunItem{Agent: lookup(si.Agent), RawInput: item, Kind: si.Type})
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
