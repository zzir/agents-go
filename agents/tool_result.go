package agents

import "encoding/json"

// ToolResult is what a tool returns: everything the run needs to know about one
// invocation, not just the value the model sees.
//
// A bare return value cannot say "show this in the UI but do not tell the
// model", or "this call cost 800 tokens of its own", or "we are done here". The
// tool knows all of it at the moment it returns.
//
// The zero value is a valid empty result. Most tools never build one by hand:
// NewTool wraps an ordinary return value automatically.
type ToolResult struct {
	// Content is what goes back to the model — text, images, files. Empty
	// content sends an empty string, which is a valid answer for a tool whose
	// effect is the point.
	Content []ToolOutputContent

	// Details is structured data for the UI and for logs. It NEVER reaches the
	// model, and it lands on the item's Display().Extra.
	//
	// It must survive a JSON round-trip; anything that cannot (NaN, channels,
	// cycles) fails the run rather than being silently dropped at serialization
	// time, when the tool call is long gone.
	Details map[string]any

	// Display names the renderer the tool would like: "diff", "terminal",
	// "table", "json", "markdown". It is a hint — a consumer that does not know
	// the name falls back to plain text rather than failing.
	Display string

	// Title is the card heading a renderer shows for this call, when the tool
	// name is not it ("Apply patch" over "apply_patch", a task's label over
	// "task_spawn"). Empty means the consumer falls back to the tool name —
	// like every display field, an override, never required. It never reaches
	// the model.
	Title string

	// Summary is the one-line account of what happened ("3 files changed"),
	// shown where the full output would drown the timeline. Empty means the
	// consumer renders what it already renders today. It never reaches the
	// model.
	Summary string

	// Usage accounts for model calls the tool made itself: an agent-as-tool's
	// nested run, a summarization step, a sub-agent. Without it that spend
	// lands in the run total with nothing to attribute it to.
	Usage *Usage

	// AddedTools names tools this result discloses to the model.
	//
	// It is how a tool opens a door: an authentication tool announcing the
	// account tools, a planner announcing the executors. Naming a tool that is
	// not marked deferred, or does not exist, is ignored — a tool should not be
	// able to fail a run by mentioning something.
	AddedTools []string

	// Terminate asks the run to stop after this batch of tools finishes.
	//
	// It takes effect only when EVERY tool in the batch asks for it. One tool
	// wanting to stop while another is still working is not a decision the SDK
	// can make for them, and stopping anyway would discard the other's result.
	Terminate bool

	// IsError marks a result that reports a failure. The content still goes to
	// the model — a tool that failed usefully says why — but the item renders
	// as an error.
	IsError bool
}

// TextResult is the common case: a tool that returns text to the model.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []ToolOutputContent{ToolOutputText{Text: text}}}
}

// WithDetails attaches UI data to a result, returning the result so it can be
// built in one expression.
func (r ToolResult) WithDetails(details map[string]any) ToolResult {
	r.Details = details
	return r
}

// WithDisplay names the renderer for a result.
func (r ToolResult) WithDisplay(renderer string) ToolResult {
	r.Display = renderer
	return r
}

// WithTitle sets the card heading for a result.
func (r ToolResult) WithTitle(title string) ToolResult {
	r.Title = title
	return r
}

// WithSummary sets the one-line account for a result.
func (r ToolResult) WithSummary(summary string) ToolResult {
	r.Summary = summary
	return r
}

// Text renders the result as the string the model would see, so a consumer
// putting it on a wire need not reimplement the string/JSON split.
func (r ToolResult) Text() string { return stringifyToolOutput(r.ModelOutput()) }

// ModelOutput renders the result's content into the value the runner sends to
// the model: a single text part collapses to its string, anything multimodal
// stays a content list.
func (r ToolResult) ModelOutput() any {
	switch len(r.Content) {
	case 0:
		return ""
	case 1:
		if t, ok := r.Content[0].(ToolOutputText); ok {
			return t.Text
		}
	}
	return r.Content
}

// resultFromValue wraps a tool's return value as a ToolResult: a ToolResult or
// *ToolResult passes through, ToolOutputContent becomes content, else stringified.
func resultFromValue(v any) ToolResult {
	switch out := v.(type) {
	case ToolResult:
		return out
	case *ToolResult:
		if out == nil {
			return ToolResult{}
		}
		return *out
	case []ToolOutputContent:
		return ToolResult{Content: out}
	case ToolOutputContent:
		return ToolResult{Content: []ToolOutputContent{out}}
	default:
		return TextResult(stringifyToolOutput(v))
	}
}

// normalizeDetails round-trips Details through JSON so an unserializable value
// fails here, not at persistence time; an empty map normalizes to nil.
func normalizeDetails(details map[string]any) (map[string]any, error) {
	if len(details) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return nil, NewUserError("ToolResult.Details is not JSON-serializable: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, NewUserError("ToolResult.Details did not survive a JSON round-trip: %v", err)
	}
	return out, nil
}
