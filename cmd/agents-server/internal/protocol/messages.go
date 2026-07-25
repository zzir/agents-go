// Package protocol defines the WebSocket envelope and the client→server / server→client message payloads exchanged between agents-server and its web UI.
package protocol

import "encoding/json"

// Envelope is the tagged wrapper for every WebSocket message: a type discriminator plus a JSON payload.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Envelope.Type values. These ARE the wire protocol — every emitter and
// consumer must reference these constants, never a string literal, so a typo
// is a compile error instead of an event the frontend silently never receives.
// The frontend mirror lives in web/frontend/src/lib/protocol.ts; keep both in
// sync when adding an event.
const (
	// Client → server
	EventAuth         = "auth"
	EventRunCreate    = "run.create"
	EventRunCancel    = "run.cancel"
	EventRunSubscribe = "run.subscribe"
	EventToolApprove  = "tool.approve"
	EventToolReject   = "tool.reject"

	// Server → client
	EventAuthOK              = "auth.ok"
	EventRunStarted          = "run.started"
	EventRunAgentStart       = "run.agent_start"
	EventRunStep             = "run.step"
	EventRunReasoning        = "run.reasoning"
	EventRunMessage          = "run.message"
	EventRunReasoningItem    = "run.reasoning_item"
	EventRunToolCall         = "run.tool_call"
	EventRunToolResult       = "run.tool_result"
	EventRunHandoff          = "run.handoff"
	EventRunOutput           = "run.output"
	EventRunError            = "run.error"
	EventRunInterrupted      = "run.interrupted"
	EventRunCancelled        = "run.cancelled"
	EventRunCompaction       = "run.compaction"
	EventRunGap              = "run.gap"
	EventSessionTitleUpdated = "session.title_updated"
	EventTraceSpan           = "trace.span"

	// Terminal events, exchanged on /ws/terminal (one terminal per
	// connection). Control frames are JSON Envelopes (text); the terminal
	// byte stream itself rides binary WebSocket frames in both directions.
	EventTerminalOpen   = "terminal.open"   // client → server
	EventTerminalResize = "terminal.resize" // client → server
	EventTerminalReady  = "terminal.ready"  // server → client
	EventTerminalError  = "terminal.error"  // server → client
	EventTerminalExit   = "terminal.exit"   // server → client
)

// RunError.Code values. Same single-point rule as the event constants: the
// frontend branches on these to pick recovery behavior, so a misspelled code
// downgrades a handled error to the generic path without any signal.
//
// Two origins share one flat namespace on the wire (PROTOCOL.md F3):
//
//   - SDK codes come from agents.CodeOf(err) and are NOT redeclared here —
//     the SDK owns that vocabulary and adds to it without a change in this
//     package. The frontend mirror lists them for exhaustive rendering only.
//   - Transport codes below describe failures that happen before or outside a
//     run, where no SDK error exists to classify.
//
// The two sets must not collide. A client that does not recognize a code falls
// back to generic error rendering, so the SDK may ship a new one first.
const (
	CodeSessionBusy     = "session_busy"
	CodeSessionNotFound = "session_not_found"
	CodeRunNotFound     = "run_not_found"
	CodeApprovalFailed  = "approval_failed"
	CodeConfigError     = "config_error"
)

// NewEnvelope marshals payload and wraps it in an Envelope of the given type.
func NewEnvelope(typ string, payload any) (*Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Envelope{Type: typ, Payload: raw}, nil
}

// Client → Server messages

// RunCreate is the client request to start a new run within a session.
type RunCreate struct {
	SessionID     string `json:"session_id"`
	Input         string `json:"input"`
	AgentConfigID string `json:"agent_config_id,omitempty"`
	SandboxID     string `json:"sandbox_id,omitempty"`
}

// RunCancel is the client request to cancel an in-flight run.
type RunCancel struct {
	RunID string `json:"run_id"`
	// Mode selects how to stop: "" / "abort" cancels immediately (mid-turn);
	// "graceful" lets the current turn finish (tools + session save) and stops
	// cleanly before the next one.
	Mode string `json:"mode,omitempty"`
}

// RunSubscribe is the client request to (re)attach to a run's event stream,
// replaying buffered events after FromSeq (0 replays everything retained).
// Used after a reconnect to resume a run that kept executing server-side.
type RunSubscribe struct {
	RunID   string `json:"run_id"`
	FromSeq int    `json:"from_seq,omitempty"`
}

// ToolApprove is the client's approval of a pending tool call awaiting human review.
type ToolApprove struct {
	ToolCallID string `json:"tool_call_id"`
	// Scope extends an exec_command approval: "once" (default), "same" (trust
	// this exact command for the session) or "all" (trust every command).
	Scope string `json:"scope,omitempty"`
}

// ToolReject is the client's rejection of a pending tool call, with an optional reason.
type ToolReject struct {
	ToolCallID string `json:"tool_call_id"`
	Reason     string `json:"reason,omitempty"`
}

// TerminalOpen is the client request that starts an interactive terminal on
// /ws/terminal; it must be the first message after auth. Cols/Rows of zero
// use the backend defaults (80x24).
type TerminalOpen struct {
	SandboxID string `json:"sandbox_id"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

// TerminalResize is the client request to change the PTY size.
type TerminalResize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// TerminalError reports why the terminal could not be opened (or died); the
// server closes the connection after sending it.
type TerminalError struct {
	Message string `json:"message"`
}

// TerminalExit reports that the shell exited. Code is -1 when unknown (e.g.
// the transport closed before an exit status was delivered).
type TerminalExit struct {
	Code int `json:"code"`
}

// Server → Client messages

// RunStarted notifies the client that a run has begun and carries its assigned run ID.
type RunStarted struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	// Input is the user prompt that started this run. Run events are broadcast
	// to every connection, and an in-flight turn is not persisted yet — a
	// browser that did not send the prompt renders its user bubble from this
	// (the sender dedups against its own optimistic bubble).
	Input string `json:"input,omitempty"`
	// Task metadata, set only for background task runs (spawn_task): the
	// parent chat session/run this task belongs to and the spawning tool call.
	// The client routes such runs into the parent session's task list instead
	// of a chat timeline (SessionID is the task's own hidden session).
	ParentSessionID string `json:"parent_session_id,omitempty"`
	ParentRunID     string `json:"parent_run_id,omitempty"`
	// TaskID is the durable task identity; RunID is this attempt's execution
	// id. Clients key task state by TaskID and route events by RunID.
	TaskID     string `json:"task_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Label      string `json:"label,omitempty"`
}

// Task status vocabulary, aligned with the MCP Tasks (SEP-1686) five-state
// lifecycle. The mapping from run status is single-pointed in
// bridge.TaskStatusFor.
const (
	TaskWorking       = "working"
	TaskInputRequired = "input_required"
	TaskCompleted     = "completed"
	TaskFailed        = "failed"
	TaskCancelled     = "cancelled"
)

// TaskNotificationPrefix marks a user-input message injected by the server
// when a background task finishes (the parent run "wakes" on it). The client
// renders such messages as task notifications rather than user bubbles; the
// model sees the prefixed text verbatim.
const TaskNotificationPrefix = "[task-notification] "

// RunAgentStart notifies the client that a (possibly handed-off-to) agent has started its turn.
type RunAgentStart struct {
	RunID     string `json:"run_id"`
	AgentName string `json:"agent_name"`
}

// RunStep streams an incremental chunk of the agent's output text.
type RunStep struct {
	RunID string `json:"run_id"`
	Delta string `json:"delta"`
}

// RunReasoning streams an incremental chunk of the agent's reasoning
// (thinking) text, when the model emits reasoning deltas.
type RunReasoning struct {
	RunID string `json:"run_id"`
	Delta string `json:"delta"`
}

// RunMessage carries one completed assistant message — the full text of a
// turn the moment the model finishes it: interim narration between tool calls
// as well as the final answer. It is the authoritative form of what run.step
// deltas previewed (and the only text signal on backends that stream no
// deltas or on resumed runs whose earlier deltas predate the resume).
type RunMessage struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
	// ItemID is the model item's stable id. The client dedups hub replays by it
	// (falling back to text equality when empty), so a genuinely repeated
	// identical message is preserved rather than dropped as a replay.
	ItemID string `json:"item_id,omitempty"`
}

// RunReasoningItem carries one completed reasoning (thinking) block — a
// turn's full thinking text the moment the model finishes it. Authoritative
// over run.reasoning deltas, and the only thinking signal on backends that
// stream no reasoning deltas or on resumed segments.
type RunReasoningItem struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
	// ItemID is the model item's stable id, used for replay dedup like RunMessage.
	ItemID string `json:"item_id,omitempty"`
}

// RunToolCall is emitted when the agent invokes a tool (or requests approval for one).
type RunToolCall struct {
	RunID         string `json:"run_id"`
	ToolCallID    string `json:"tool_call_id"`
	ToolName      string `json:"tool_name"`
	Arguments     string `json:"arguments"`
	NeedsApproval bool   `json:"needs_approval"`
}

// RunToolResult carries the output of a completed tool call back to the client.
type RunToolResult struct {
	RunID      string `json:"run_id"`
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
}

// RunHandoff is emitted when control transfers from one agent to another.
type RunHandoff struct {
	RunID string `json:"run_id"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// RunOutput carries the run's final output once the agent has finished.
type RunOutput struct {
	RunID       string `json:"run_id"`
	FinalOutput string `json:"final_output"`
}

// RunError reports that a run failed, with an error code and message.
// SessionID is set when the failure happened before a run.started could
// establish the run→session mapping (e.g. session_not_found, session_busy),
// so the client can still attribute the error.
type RunError struct {
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	// Guardrail and Stage are set only when Code is "guardrail_tripwire": the
	// name of the guardrail that blocked the run and whether it fired on the
	// input ("input") or the final output ("output"). An output trip means the
	// answer already streamed to the client and should be marked retracted.
	Guardrail string `json:"guardrail,omitempty"`
	Stage     string `json:"stage,omitempty"`
}

// RunInterrupted signals that the run paused for human tool approval. It is
// terminal for this run segment's event stream — approving or rejecting
// resumes execution under the same run id.
type RunInterrupted struct {
	RunID string `json:"run_id"`
}

// RunCancelled notifies the client that a run was cancelled.
type RunCancelled struct {
	RunID string `json:"run_id"`
}

// RunCompaction reports session-history compaction progress at the end of a
// run: phase "started" when the summarization request begins (the run stays
// busy until it completes), "finished" with item counts once history is
// rewritten. Transient status — not persisted to traces.
type RunCompaction struct {
	RunID  string `json:"run_id"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
}

// RunGap tells one connection that it fell behind and events were dropped for
// it. Only that connection receives it; the run itself is unaffected.
//
// It exists because the alternative is worse. Dropping silently leaves a
// timeline quietly missing a tool result, which looks exactly like one that
// never had it; disconnecting punishes a user for a slow render. The client
// resubscribes with from_seq = last_good to fill the hole.
type RunGap struct {
	RunID string `json:"run_id"`
	// Dropped is how many events were discarded for this connection.
	Dropped int `json:"dropped"`
	// LastGood is the sequence number to resubscribe from.
	LastGood int `json:"last_good"`
	// Next is the sequence number of the event delivered right after the gap.
	Next int `json:"next"`
}

// Session events

// SessionTitleUpdated notifies the client that a session's title has changed.
type SessionTitleUpdated struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

// Tracing events

// TraceSpan carries a single tracing span (its IDs, name, type, timing, error
// state, and data) to the client.
type TraceSpan struct {
	RunID     string         `json:"run_id"`
	TraceID   string         `json:"trace_id"`
	SpanID    string         `json:"span_id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	Error     string         `json:"error,omitempty"`
	StartedAt string         `json:"started_at"`
	EndedAt   string         `json:"ended_at,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}
