// Package protocol defines the WebSocket envelope and the client→server /
// server→client message payloads exchanged between agents-server and its web
// UI.
package protocol

import (
	"encoding/json"
	"time"

	"github.com/zzir/agents-go/agents/tasks"
)

// Envelope is the tagged wrapper for every WebSocket message: a type
// discriminator plus a JSON payload.
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
	// EventRunInject delivers input to a live run (queue: the InjectQueue* constants).
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
	// EventRunToolProgress is a partial result a running tool pushed;
	// run.tool_result replaces it.
	EventRunToolProgress = "run.tool_progress"
	EventRunToolResult   = "run.tool_result"
	EventRunHandoff      = "run.handoff"
	EventRunOutput       = "run.output"
	EventRunError        = "run.error"
	EventRunInterrupted  = "run.interrupted"
	EventRunCancelled    = "run.cancelled"
	EventRunCompaction   = "run.compaction"
	// EventRunInjected says the run read input queued on it (run.inject).
	EventRunInjected = "run.injected"
	// EventRunDiagnostic reports trouble a run survived (retries, a fallback model).
	EventRunDiagnostic       = "run.diagnostic"
	EventRunGap              = "run.gap"
	EventSessionTitleUpdated = "session.title_updated"
	// EventSessionProjectBound announces the run that bound a project to the session.
	EventSessionProjectBound = "session.project_bound"
	// EventSessionStatus carries a conversation's derived status to its owner's
	// connections; never replayed.
	EventSessionStatus = "session.status"
	// EventTaskUpdated rides the task run's stream and carries the parent id
	// and the task row.
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

// RunError.Code values the workbench adds (invariant 15); SDK codes come from
// agents.CodeOf(err). The whole vocabulary is docs/reference/protocol.md, "Run
// error codes".
const (
	CodeSessionBusy     = "session_busy"
	CodeSessionNotFound = "session_not_found"
	CodeRunNotFound     = "run_not_found"
	CodeApprovalFailed  = "approval_failed"
	CodeConfigError     = "config_error"
	CodePersistError    = "persist_error"
	CodeStreamError     = "stream_error"
	CodeResumeError     = "resume_error"
	CodeProviderError   = "provider_error"
	CodeContextOverflow = "context_overflow"
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

// AttachmentRef is one image attachment as run events carry it, enough to
// render the thumbnail.
type AttachmentRef struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// RunCreate is the client request to start a run; ProjectID matters only until
// the session binds one.
type RunCreate struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
	// AttachmentIDs name uploaded images to send; chat runs with Vision only.
	AttachmentIDs []string `json:"attachment_ids,omitempty"`
	AgentConfigID string   `json:"agent_config_id,omitempty"`
	ProjectID     string   `json:"project_id,omitempty"`
	// Plan: true enters the plan phase, false leaves it, absent keeps it.
	Plan *bool `json:"plan,omitempty"`
}

// RunCancel is the client request to cancel an in-flight run.
type RunCancel struct {
	RunID string `json:"run_id"`
	// Mode: "" / "abort" cancels mid-turn; "graceful" stops before the next turn.
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

// RunSubscribe (re)attaches to a run's event stream, replaying after FromSeq
// (0: everything retained).
type RunSubscribe struct {
	RunID   string `json:"run_id"`
	FromSeq int    `json:"from_seq,omitempty"`
}

// ToolApprove is the client's approval of a pending tool call awaiting human review.
type ToolApprove struct {
	ToolCallID string `json:"tool_call_id"`
	// Scope extends an exec_command approval: "once" (default), "same" or "all".
	Scope string `json:"scope,omitempty"`
}

// ToolReject is the client's rejection of a pending tool call, with an optional reason.
type ToolReject struct {
	ToolCallID string `json:"tool_call_id"`
	Reason     string `json:"reason,omitempty"`
}

// TerminalOpen is the first message on /ws/terminal after auth; zero Cols/Rows
// mean 80x24.
type TerminalOpen struct {
	// ProjectID selects the container to open the shell in.
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
// connection closes after it.
type TerminalError struct {
	Message string `json:"message"`
}

// TerminalExit reports that the shell exited; Code is -1 when unknown.
type TerminalExit struct {
	Code int `json:"code"`
}

// Server → Client messages

// RunStarted notifies the client that a run has begun and carries its assigned run ID.
type RunStarted struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	// Input is the prompt that started the run, for a browser that did not send it.
	Input string `json:"input,omitempty"`
	// Attachments are the message's images, for the same reason as Input.
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	// Task metadata, set only for background task runs (SessionID is the task's
	// hidden session).
	ParentSessionID string `json:"parent_session_id,omitempty"`
	ParentRunID     string `json:"parent_run_id,omitempty"`
	// TaskID is the durable task identity; RunID this attempt's execution id.
	TaskID     string `json:"task_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Label      string `json:"label,omitempty"`
	// Kind is the task's kind: "" a sub-agent task, "workflow" an execution's step run.
	Kind string `json:"kind,omitempty"`
	// Attempt is which run of the task this is: 1 for the original, more after a retry.
	Attempt int `json:"attempt,omitempty"`
	// MaxAttempts is the ceiling Attempt is measured against.
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

// TaskNotificationPrefix marks the user-input message injected when a
// background task finishes; aliased from the SDK.
const TaskNotificationPrefix = tasks.NotificationPrefix

// RunToolProgress is a partial result from a tool still running, keyed by CallID.
type RunToolProgress struct {
	RunID    string `json:"run_id"`
	CallID   string `json:"call_id"`
	ToolName string `json:"tool_name"`
	// Delta is the partial output, appended to what the client has for this call.
	Delta string `json:"delta"`
	// Renderer is the tool's display hint (e.g. "terminal").
	Renderer string `json:"renderer,omitempty"`
}

// RunDiagnostic reports one piece of trouble a run survived.
type RunDiagnostic struct {
	RunID string `json:"run_id"`
	// Type is the diagnostic kind (model_retry, model_fallback, tool_panic, …),
	// an open vocabulary.
	Type string `json:"type"`
	// Code is the classified error, when there was one.
	Code string `json:"code,omitempty"`
	// Message is a one-line summary.
	Message string `json:"message,omitempty"`
	// Details carries type-specific fields (attempt number, model, …).
	Details map[string]any `json:"details,omitempty"`
}

// RunAgentStart notifies the client that a (possibly handed-off-to) agent has
// started its turn.
type RunAgentStart struct {
	RunID     string `json:"run_id"`
	AgentName string `json:"agent_name"`
	// AgentConfigID is the config row behind the named agent, for its avatar;
	// empty when none.
	AgentConfigID string `json:"agent_config_id,omitempty"`
}

// RunStep streams an incremental chunk of the agent's output text.
type RunStep struct {
	RunID string `json:"run_id"`
	Delta string `json:"delta"`
}

// RunReasoning streams an incremental chunk of the agent's reasoning text.
type RunReasoning struct {
	RunID string `json:"run_id"`
	Delta string `json:"delta"`
}

// RunMessage carries one completed assistant message, the authoritative form
// of what run.step deltas previewed.
type RunMessage struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
	// ItemID is the model item's stable id, for replay dedup (text equality
	// when empty).
	ItemID string `json:"item_id,omitempty"`
}

// RunReasoningItem carries one completed reasoning block, authoritative over
// run.reasoning deltas.
type RunReasoningItem struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
	// ItemID is the model item's stable id, used for replay dedup like RunMessage.
	ItemID string `json:"item_id,omitempty"`
}

// RunInjected carries one injected input the run has read; Index counts the
// run's injections from 1.
type RunInjected struct {
	RunID string `json:"run_id"`
	Input string `json:"input"`
	Index int    `json:"index"`
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
	// Title and Summary are the tool's display overrides; empty keeps the
	// card's fallbacks.
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Renderer is the tool's rendering hint ("diff", "terminal", …), as in
	// RunToolProgress.
	Renderer string `json:"renderer,omitempty"`
	// IsError marks a result that reports a failure.
	IsError bool `json:"is_error,omitempty"`
	// Extra is whatever the tool attached via ToolResult.Details (a task_id, a
	// command).
	Extra map[string]any `json:"extra,omitempty"`
}

// RunHandoff is emitted when control transfers from one agent to another.
type RunHandoff struct {
	RunID string `json:"run_id"`
	From  string `json:"from"`
	To    string `json:"to"`
	// FromID/ToID name the config rows behind the agents, for avatars; empty when none.
	FromID string `json:"from_id,omitempty"`
	ToID   string `json:"to_id,omitempty"`
}

// RunOutput carries the run's final output once the agent has finished.
type RunOutput struct {
	RunID       string `json:"run_id"`
	FinalOutput string `json:"final_output"`
}

// RunError reports a failed run; SessionID is set when the failure came before
// run.started (session_not_found, session_busy).
type RunError struct {
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	// Guardrail and Stage ("input" / "output") are set only for guardrail_tripwire.
	Guardrail string `json:"guardrail,omitempty"`
	Stage     string `json:"stage,omitempty"`
}

// RunInterrupted signals that the run paused for tool approval; a decision
// resumes the same run id.
type RunInterrupted struct {
	RunID string `json:"run_id"`
}

// RunCancelled notifies the client that a run was cancelled.
type RunCancelled struct {
	RunID string `json:"run_id"`
	// Reason is stopped, superseded or shutdown (the constants below).
	Reason string `json:"reason,omitempty"`
}

// The run.cancelled reasons.
const (
	RunCancelStopped    = "stopped"
	RunCancelSuperseded = "superseded"
	RunCancelShutdown   = "shutdown"
)

// RunCompaction reports compaction progress at the end of a run: phase
// "started", then "finished" with item counts. Transient, not persisted.
type RunCompaction struct {
	RunID  string `json:"run_id"`
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
}

// RunGap tells one connection that events were dropped for it; it resubscribes
// from last_good.
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

// SessionProjectBound notifies the client that the session is now bound to project_id.
type SessionProjectBound struct {
	SessionID string `json:"session_id"`
	ProjectID string `json:"project_id"`
}

// A conversation's derived status, highest priority first.
const (
	SessionRequiresAction = "requires_action"
	SessionRunning        = "running"
	SessionFailed         = "failed"
	SessionIdle           = "idle"
)

// SessionStatus is a conversation's status as GET /sessions reports it.
type SessionStatus struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	// LiveRunID is the conversation's own executing run; a paused run is not live.
	LiveRunID    string `json:"live_run_id,omitempty"`
	PendingCount int    `json:"pending_count"`
	// OldestPendingAt is when the longest-waiting decision was asked for.
	OldestPendingAt *time.Time `json:"oldest_pending_at,omitempty"`
}

// TaskUpdated is a task's state as the parent session's subscribers show it:
// a row of GET /sessions/{id}/tasks, merged under the task id.
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
	// PendingCallID / PendingToolName name the decision an input_required task
	// waits on.
	PendingCallID   string `json:"pending_call_id,omitempty"`
	PendingToolName string `json:"pending_tool_name,omitempty"`
	// Dismissed is the row's hidden-from-the-strip flag.
	Dismissed bool      `json:"dismissed,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Tracing events

// TraceSpan carries one tracing span to the client.
type TraceSpan struct {
	RunID string `json:"run_id"`
	// ParentRunID is the run's lineage (a wake-up run's spawning run), as
	// stored trace rows carry it.
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
	// PayloadOmitted marks Data whose payload fields the live cap replaced; the
	// stored row has them.
	PayloadOmitted bool `json:"payload_omitted,omitempty"`
	// Attachments are the image attachments the span's input items reference, resolved.
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}
