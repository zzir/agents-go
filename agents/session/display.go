package session

// ItemDisplay is an item projected into what a renderer actually needs. It is
// produced by the layer that knows the wire format — the runner for live
// items, the projection for stored entries — rather than by each consumer
// parsing the wire again.
//
// It is a hint, not a replacement: a consumer that ignores ItemDisplay
// entirely must still be able to render from the underlying item, which is
// what keeps ItemDisplay free to gain fields without breaking anyone.
type ItemDisplay struct {
	// Kind is the item kind: message, tool_call, tool_output, reasoning,
	// handoff, unknown. An unrecognized kind must fall back, not fail.
	Kind string `json:"kind"`
	// Renderer is a tool's requested renderer ("diff", "terminal", "table", …),
	// from ToolResult.Display. A consumer that does not know the name falls
	// back to plain text rather than failing.
	Renderer string `json:"renderer,omitzero"`
	// Title is the card heading, when the tool name is not it — a task's
	// label, "Apply patch" over "apply_patch". Empty means fall back to
	// ToolName: an override, never required, per the display contract.
	Title string `json:"title,omitzero"`
	// Summary is the one-line account of what happened ("3 files changed"),
	// for where the full Output would drown the timeline. Empty means render
	// what the other fields already carry.
	Summary string `json:"summary,omitzero"`
	// Text is the human-readable body: a message's text, a reasoning summary.
	Text string `json:"text,omitzero"`
	// CallID ties a tool call to its output.
	CallID string `json:"call_id,omitzero"`
	// ToolName is the tool being called, or the handoff tool.
	ToolName string `json:"tool_name,omitzero"`
	// Arguments is the raw JSON the model passed to the tool.
	Arguments string `json:"arguments,omitzero"`
	// Output is a tool result rendered as text.
	Output string `json:"output,omitzero"`
	// IsError marks a tool result that reports a failure.
	IsError bool `json:"is_error,omitzero"`
	// Extra carries whatever a tool's CustomDataExtractor produced. It is
	// SDK-side only and never reaches the model.
	Extra map[string]any `json:"extra,omitzero"`
}

// Display kinds.
const (
	DisplayMessage    = "message"
	DisplayToolCall   = "tool_call"
	DisplayToolOutput = "tool_output"
	DisplayReasoning  = "reasoning"
	DisplayHandoff    = "handoff"
	DisplayUnknown    = "unknown"
	// DisplayError and DisplayCancelled are what an annotation entry renders
	// as. They are display kinds rather than item kinds because nothing was
	// said: they report on the run, and the model never reads them.
	DisplayError     = "error"
	DisplayCancelled = "cancelled"
)
