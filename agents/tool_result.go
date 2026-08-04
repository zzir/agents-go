package agents

import "encoding/json"

// ToolResult is what a tool returns: everything the run needs to know about one
// invocation, not just the value the model sees.
//
// A bare return value cannot say "show this in the UI but do not tell the
// model", or "this call cost 800 tokens of its own", or "we are done here". The
// SDK used to reach for those out of band — a CustomDataExtractor that ran a
// second pass over the finished call to produce UI data, and a consumer-side
// patch applied afterwards to attach it. The tool already knows all of it at
// the moment it returns.
//
// The zero value is a valid empty result. Most tools never build one by hand:
// NewFunctionTool wraps an ordinary return value automatically.
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

// Text renders the result as the string the model would see.
//
// It exists so a consumer that has to put a result on a wire — a UI event, a
// log line — does not have to reimplement the string/JSON split, and get it
// subtly different from what the model was actually sent.
func (r ToolResult) Text() string { return stringifyToolOutput(r.ModelOutput()) }

// ModelOutput renders the result's content into the value the runner sends to
// the model: a single text part collapses to its string (the overwhelmingly
// common case, and what a tool returning a plain value produced before), while
// anything multimodal stays a content list.
//
// A wrapper around another tool uses it to see what the model will receive
// without reimplementing the collapse rule.
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

// resultFromValue wraps an ordinary tool return value as a ToolResult, so a
// tool that just returns a string or a struct keeps working unchanged.
//
// A value that is already a ToolResult passes through; one that implements
// ToolOutputContent (or is a slice of it) becomes the content directly;
// everything else is stringified the way tool output always has been.
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

// normalizeDetails round-trips a result's Details through JSON so a value that
// cannot be serialized fails here — while the tool call that produced it is
// still identifiable — rather than at persistence time, long after.
//
// An empty map normalizes to nil so an absent value and an empty one look the
// same to consumers.
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
