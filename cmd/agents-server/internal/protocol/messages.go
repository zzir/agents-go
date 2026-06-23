// Package protocol defines the WebSocket envelope and the client→server / server→client message payloads exchanged between agents-server and its web UI.
package protocol

import "encoding/json"

// Envelope is the tagged wrapper for every WebSocket message: a type discriminator plus a JSON payload.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

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
}

// ToolApprove is the client's approval of a pending tool call awaiting human review.
type ToolApprove struct {
	ToolCallID string `json:"tool_call_id"`
}

// ToolReject is the client's rejection of a pending tool call, with an optional reason.
type ToolReject struct {
	ToolCallID string `json:"tool_call_id"`
	Reason     string `json:"reason,omitempty"`
}

// Server → Client messages

// RunStarted notifies the client that a run has begun and carries its assigned run ID.
type RunStarted struct {
	RunID string `json:"run_id"`
}

// RunAgentStart notifies the client that a (possibly handed-off-to) agent has started its turn.
type RunAgentStart struct {
	AgentName string `json:"agent_name"`
}

// RunStep streams an incremental chunk of the agent's output text.
type RunStep struct {
	Delta string `json:"delta"`
}

// RunToolCall is emitted when the agent invokes a tool (or requests approval for one).
type RunToolCall struct {
	ToolCallID    string `json:"tool_call_id"`
	ToolName      string `json:"tool_name"`
	Arguments     string `json:"arguments"`
	NeedsApproval bool   `json:"needs_approval"`
}

// RunToolResult carries the output of a completed tool call back to the client.
type RunToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
}

// RunHandoff is emitted when control transfers from one agent to another.
type RunHandoff struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RunOutput carries the run's final output once the agent has finished.
type RunOutput struct {
	FinalOutput string `json:"final_output"`
}

// RunError reports that a run failed, with an error code and message.
type RunError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Session events

// SessionTitleUpdated notifies the client that a session's title has changed.
type SessionTitleUpdated struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

// Hook lifecycle events

// HookEvent reports a runner lifecycle hook firing (agent start/end, tool start/end, handoff, etc.).
type HookEvent struct {
	Hook      string `json:"hook"`
	AgentName string `json:"agent_name,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Tracing events

// TraceSpan carries a single tracing span (its IDs, name, type, timing, and data) to the client.
type TraceSpan struct {
	TraceID   string         `json:"trace_id"`
	SpanID    string         `json:"span_id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	StartedAt string         `json:"started_at"`
	EndedAt   string         `json:"ended_at,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}
