// Package protocol defines the WebSocket envelope and the client→server / server→client message payloads exchanged between agents-server and its web UI.
package protocol

import (
	"encoding/json"
	"time"

	"github.com/zzir/agents-go/agents/tasks"
)

// Envelope is the tagged wrapper for every WebSocket message: a type discriminator plus a JSON payload.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Envelope.Type values — the wire protocol, mirrored in
// web/frontend/src/lib/protocol.ts (invariant 15).
const (
	// Client → server
	EventAuth         = "auth"
	EventRunCreate    = "run.create"
	EventRunCancel    = "run.cancel"
	EventRunSubscribe = "run.subscribe"
	// EventRunInject delivers input to a live run; the payload's queue field
	// names the injection semantics (see the InjectQueue* constants).
	EventRunInject   = "run.inject"
	EventToolApprove = "tool.approve"
	EventToolReject  = "tool.reject"

	// Server → client
	EventAuthOK           = "auth.ok"
	EventRunStarted       = "run.started"
	EventRunAgentStart    = "run.agent_start"
	EventRunStep          = "run.step"
	EventRunReasoning     = "run.reasoning"
	EventRunMessage       = "run.message"
	EventRunReasoningItem = "run.reasoning_item"
	EventRunToolCall      = "run.tool_call"
	// EventRunToolProgress carries a partial result a tool pushed while still
	// running; the answer arrives as run.tool_result and replaces it.
	EventRunToolProgress = "run.tool_progress"
	EventRunToolResult   = "run.tool_result"
	EventRunHandoff      = "run.handoff"
	EventRunOutput       = "run.output"
	EventRunError        = "run.error"
	EventRunInterrupted  = "run.interrupted"
	EventRunCancelled    = "run.cancelled"
	EventRunCompaction   = "run.compaction"
	// EventRunDiagnostic reports trouble a run went through and SURVIVED
	// (retries, a fallback model, a compaction pass that gave up).
	EventRunDiagnostic       = "run.diagnostic"
	EventRunGap              = "run.gap"
	EventSessionTitleUpdated = "session.title_updated"
	// EventSessionProjectBound announces that a session's first
	// project-carrying run bound a project to it — once, by the run that won.
	EventSessionProjectBound = "session.project_bound"
	// EventTaskUpdated tells a parent session's subscribers that a background
	// task changed state. It rides the TASK run's stream, carries the parent
	// id, and its payload is the row as the tasks list returns it.
	EventTaskUpdated = "task.updated"
	EventTraceSpan   = "trace.span"

	// Terminal events, exchanged on /ws/terminal: control frames are JSON
	// Envelopes (text); the byte stream rides binary frames both ways.
	EventTerminalOpen   = "terminal.open"   // client → server
	EventTerminalResize = "terminal.resize" // client → server
	EventTerminalReady  = "terminal.ready"  // server → client
	EventTerminalError  = "terminal.error"  // server → client
	EventTerminalExit   = "terminal.exit"   // server → client
)

// RunError.Code values (invariant 15). SDK codes come from agents.CodeOf(err)
// and are NOT redeclared here; the transport codes below describe failures
// before or outside a run. The two sets must not collide; a client falls back
// to generic rendering on a code it does not know.
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

// AttachmentRef is one image attachment as run events carry it: enough for a
// client to render the thumbnail without a second request.
type AttachmentRef struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// RunCreate is the client request to start a new run within a session.
// ProjectID only matters until the session's first project-carrying run
// binds it permanently.
type RunCreate struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
	// AttachmentIDs name uploaded images (POST /attachments) to send with the
	// message; chat runs only, and only with the agent's Vision flag on.
	AttachmentIDs []string `json:"attachment_ids,omitempty"`
	AgentConfigID string   `json:"agent_config_id,omitempty"`
	ProjectID     string   `json:"project_id,omitempty"`
	// Plan asks of the session's plan phase: true enters it, false leaves it,
	// and ABSENT leaves the phase as it stands.
	Plan *bool `json:"plan,omitempty"`
}

// RunCancel is the client request to cancel an in-flight run.
type RunCancel struct {
	RunID string `json:"run_id"`
	// Mode selects how to stop: "" / "abort" cancels mid-turn; "graceful"
	// lets the current turn finish and stops before the next one.
	Mode string `json:"mode,omitempty"`
}

// The injection queues a RunInject can name: steer changes course inside the
// running turn, next-turn rides along with the next, follow-up starts a new exchange.
const (
	InjectQueueSteer    = "steer"
	InjectQueueNextTurn = "next_turn"
	InjectQueueFollowUp = "follow_up"
)

// RunInject is the client request to deliver input to a live run through the
// named queue.
type RunInject struct {
	RunID string `json:"run_id"`
	Queue string `json:"queue"`
	Input string `json:"input"`
}

// RunSubscribe is the client request to (re)attach to a run's event stream,
// replaying buffered events after FromSeq (0 replays everything retained).
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

// TerminalOpen is the first message on /ws/terminal after auth. Cols/Rows of
// zero use the backend defaults (80x24).
type TerminalOpen struct {
	// ProjectID selects the container to open the shell in — the project's
	// own, the same one a session bound to it uses.
	ProjectID string `json:"project_id"`
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
	// Input is the user prompt that started this run, so a browser that did
	// not send it can render the user bubble (the sender dedups its own).
	Input string `json:"input,omitempty"`
	// Attachments are the message's images, for the same reason Input rides
	// here: a browser that did not send them still renders the thumbnails.
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	// Task metadata, set only for background task runs: the parent chat
	// session/run and the spawning tool call (SessionID is the task's own hidden session).
	ParentSessionID string `json:"parent_session_id,omitempty"`
	ParentRunID     string `json:"parent_run_id,omitempty"`
	// TaskID is the durable task identity; RunID is this attempt's execution
	// id. Clients key task state by TaskID and route events by RunID.
	TaskID     string `json:"task_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Label      string `json:"label,omitempty"`
	// Kind is the task's kind ("" a sub-agent task, "workflow" an execution's
	// step run): a step ending is not a workflow ending.
	Kind string `json:"kind,omitempty"`
	// Attempt is which run of the task this is: 1 for the original, more
	// after a retry — how a client tells a NEW attempt from a replay.
	Attempt int `json:"attempt,omitempty"`
	// MaxAttempts is the ceiling Attempt is measured against, so a client can
	// answer "could this be retried" from state it already tracks.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// Task status vocabulary, aligned with the MCP Tasks (SEP-1686) five-state
// lifecycle; the mapping from run status is bridge.TaskStatusFor.
const (
	TaskWorking       = "working"
	TaskInputRequired = "input_required"
	TaskCompleted     = "completed"
	TaskFailed        = "failed"
	TaskCancelled     = "cancelled"
)

// TaskNotificationPrefix marks a user-input message injected when a
// background task finishes; the client renders it as a notification, the
// model sees it verbatim. Aliased from the SDK so it cannot drift.
const TaskNotificationPrefix = tasks.NotificationPrefix

// RunToolProgress is a partial result from a tool that is still running,
// keyed by CallID because several calls of one tool may stream at once.
type RunToolProgress struct {
	RunID    string `json:"run_id"`
	CallID   string `json:"call_id"`
	ToolName string `json:"tool_name"`
	// Delta is the partial output. It is appended to whatever the client has
	// for this call, not a replacement.
	Delta string `json:"delta"`
	// Renderer is the tool's display hint (e.g. "terminal").
	Renderer string `json:"renderer,omitempty"`
}

// RunDiagnostic reports one piece of trouble a run survived.
type RunDiagnostic struct {
	RunID string `json:"run_id"`
	// Type is the diagnostic kind (model_retry, model_fallback, tool_panic, …),
	// an open vocabulary: an unknown one renders generically.
	Type string `json:"type"`
	// Code is the classified error, when there was one.
	Code string `json:"code,omitempty"`
	// Message is a one-line summary.
	Message string `json:"message,omitempty"`
	// Details carries type-specific fields (attempt number, model, …).
	Details map[string]any `json:"details,omitempty"`
}

// RunAgentStart notifies the client that a (possibly handed-off-to) agent has started its turn.
type RunAgentStart struct {
	RunID     string `json:"run_id"`
	AgentName string `json:"agent_name"`
	// AgentConfigID is the config row behind the named agent, so the client
	// can render its avatar; empty when the name resolves to no config.
	AgentConfigID string `json:"agent_config_id,omitempty"`
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

// RunMessage carries one completed assistant message — interim narration or
// the final answer — the authoritative form of what run.step deltas
// previewed, and the only text signal on backends that stream no deltas.
type RunMessage struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
	// ItemID is the model item's stable id; the client dedups hub replays by
	// it, falling back to text equality when empty.
	ItemID string `json:"item_id,omitempty"`
}

// RunReasoningItem carries one completed reasoning (thinking) block,
// authoritative over run.reasoning deltas.
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

// RunToolResult carries the output of a completed tool call. The display
// fields mirror the stored entry's ItemDisplay (same JSON names).
type RunToolResult struct {
	RunID      string `json:"run_id"`
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	// Title and Summary are the tool's display overrides (ToolResult.Title /
	// .Summary); empty means the card keeps its fallbacks.
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Renderer is the tool's rendering hint for the output ("diff",
	// "terminal", …), same contract as RunToolProgress.
	Renderer string `json:"renderer,omitempty"`
	// IsError marks a result that reports a failure.
	IsError bool `json:"is_error,omitempty"`
	// Extra carries whatever the tool attached via ToolResult.Details — the
	// card's data (a task result's task_id, an exec_command's command).
	Extra map[string]any `json:"extra,omitempty"`
}

// RunHandoff is emitted when control transfers from one agent to another.
type RunHandoff struct {
	RunID string `json:"run_id"`
	From  string `json:"from"`
	To    string `json:"to"`
	// FromID/ToID name the config rows behind the agents, for avatars;
	// empty when a name resolves to no config.
	FromID string `json:"from_id,omitempty"`
	ToID   string `json:"to_id,omitempty"`
}

// RunOutput carries the run's final output once the agent has finished.
type RunOutput struct {
	RunID       string `json:"run_id"`
	FinalOutput string `json:"final_output"`
}

// RunError reports that a run failed, with an error code and message.
// SessionID is set when the failure happened before a run.started could
// establish the run→session mapping (e.g. session_not_found, session_busy).
type RunError struct {
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	// Guardrail and Stage ("input" / "output") are set only when Code is
	// "guardrail_tripwire"; an output trip means the streamed answer is retracted.
	Guardrail string `json:"guardrail,omitempty"`
	Stage     string `json:"stage,omitempty"`
}

// RunInterrupted signals that the run paused for human tool approval;
// approving or rejecting resumes execution under the same run id.
type RunInterrupted struct {
	RunID string `json:"run_id"`
}

// RunCancelled notifies the client that a run was cancelled.
type RunCancelled struct {
	RunID string `json:"run_id"`
}

// RunCompaction reports compaction progress at the end of a run: phase
// "started" when the summarization request begins, "finished" with item
// counts once history is rewritten. Transient — not persisted to traces.
type RunCompaction struct {
	RunID  string `json:"run_id"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
}

// RunGap tells one connection that it fell behind and events were dropped
// for it; the client resubscribes with from_seq = last_good to fill the hole.
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

// SessionProjectBound notifies the client that the session is now permanently
// bound to project_id.
type SessionProjectBound struct {
	SessionID string `json:"session_id"`
	ProjectID string `json:"project_id"`
}

// TaskUpdated is a task's state as its parent session's subscribers should
// show it — the same shape as a row of GET /sessions/{id}/tasks, Dismissed
// included. A client merges it under the task id, never moving backwards.
type TaskUpdated struct {
	TaskID          string          `json:"task_id"`
	ParentSessionID string          `json:"parent_session_id"`
	ParentRunID     string          `json:"parent_run_id,omitempty"`
	ToolCallID      string          `json:"tool_call_id,omitempty"`
	ChildSessionID  string          `json:"child_session_id,omitempty"`
	Kind            string          `json:"kind,omitempty"`
	Label           string          `json:"label,omitempty"`
	Status          string          `json:"status"`
	Attempt         int             `json:"attempt,omitempty"`
	MaxAttempts     int             `json:"max_attempts,omitempty"`
	Summary         string          `json:"summary,omitempty"`
	State           json.RawMessage `json:"state,omitempty"`
	// PendingCallID / PendingToolName name the decision an input_required
	// task waits on, which a pause with no run (a step waiting to start) has no run event for.
	PendingCallID   string `json:"pending_call_id,omitempty"`
	PendingToolName string `json:"pending_tool_name,omitempty"`
	// Dismissed is the row's hidden-from-the-strip flag, so a dismissal made
	// in one window reaches the others.
	Dismissed bool      `json:"dismissed,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Tracing events

// TraceSpan carries a single tracing span (its IDs, name, type, timing, error
// state, and data) to the client.
type TraceSpan struct {
	RunID string `json:"run_id"`
	// ParentRunID is the run's lineage (a wake-up run's spawning run), so the
	// live client groups runs the same way stored trace rows do.
	ParentRunID string         `json:"parent_run_id,omitempty"`
	TraceID     string         `json:"trace_id"`
	SpanID      string         `json:"span_id"`
	ParentID    string         `json:"parent_id,omitempty"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Error       string         `json:"error,omitempty"`
	StartedAt   string         `json:"started_at"`
	EndedAt     string         `json:"ended_at,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
	// PayloadOmitted marks Data whose payload fields were replaced by the live
	// cap's marker; the stored row (GET /sessions/:id/traces/:span_id) has them.
	PayloadOmitted bool `json:"payload_omitted,omitempty"`
}
