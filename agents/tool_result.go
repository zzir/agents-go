package agents

import "encoding/json"

// ToolResult is what a tool returns: the content the model sees plus what the
// run needs to know about the invocation — see spec §2.7b. The zero value is a
// valid empty result; NewTool wraps an ordinary return value automatically.
type ToolResult struct {
	// Content is what goes back to the model: text, images, files. Empty sends "".
	Content []ToolOutputContent

	// Details is structured data for the UI and logs; it never reaches the
	// model and must survive a JSON round-trip.
	Details map[string]any

	// Display names the renderer the tool would like ("diff", "terminal",
	// "table", "json", "markdown"); an unknown name falls back to plain text.
	Display string

	// Title overrides the tool name as the card heading; never reaches the model.
	Title string

	// Summary is a one-line account of what happened; never reaches the model.
	Summary string

	// Usage accounts for model calls the tool made itself (a nested run, a
	// summarization step).
	Usage *Usage

	// AddedTools names deferred tools this result discloses to the model; an
	// unknown or non-deferred name is ignored — see spec §2.7i.
	AddedTools []string

	// Terminate asks the run to stop after this batch of tools; it takes
	// effect only when every tool in the batch asks.
	Terminate bool

	// IsError marks a failed result for renderers; the content still reaches the model.
	IsError bool
}

// TextResult is the common case: a tool that returns text to the model.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []ToolOutputContent{ToolOutputText{Text: text}}}
}

// WithDetails attaches UI data to a result and returns it for chaining.
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

// Text renders the result as the string the model would see.
func (r ToolResult) Text() string { return stringifyToolOutput(r.ModelOutput()) }

// ModelOutput renders the content as the runner sends it: a single text part
// collapses to its string, anything multimodal stays a content list.
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

// normalizeDetails round-trips Details through JSON; an empty map normalizes to nil.
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
