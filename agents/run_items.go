package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"
)

// RunItem is a sealed interface for the items produced during a run: model
// messages, tool calls, tool outputs, handoffs and reasoning. Every item knows
// the agent that produced it and can be converted to a model input item for the
// next turn. It mirrors the Python SDK's RunItem union.
type RunItem interface {
	// AgentRef returns the agent associated with this item.
	AgentRef() *Agent
	// ToInputItem converts the item to a Responses API input item.
	ToInputItem() (TResponseInputItem, error)
	// ItemType returns a stable discriminator string (e.g. "message_output").
	ItemType() string
	// Source reports who produced the item. The zero value is the model.
	Source() Source
	// Display projects the item into the fields a UI needs, so consumers do not
	// parse the Responses wire format themselves.
	Display() ItemDisplay
	isRunItem()
}

// ItemDisplay is an item projected into what a renderer actually needs: the
// text, the tool call, the error flag. It is produced by the SDK, which knows
// the wire format, rather than by each consumer parsing it again — the version
// of that parsing living in agents-server was the source of a long tail of
// rendering bugs.
//
// It is a hint, not a replacement. A consumer that ignores Display entirely
// must still be able to render correctly from the item's own fields; that is
// what keeps Display free to gain fields without breaking anyone.
type ItemDisplay struct {
	// Kind is the item kind: message, tool_call, tool_output, reasoning,
	// handoff, unknown. An unrecognized kind must fall back, not fail.
	Kind string `json:"kind"`
	// Renderer is a tool's requested renderer ("diff", "terminal", "table", …),
	// from ToolResult.Display. A consumer that does not know the name falls
	// back to plain text rather than failing.
	Renderer string `json:"renderer,omitzero"`
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

// ItemDisplay kinds.
const (
	DisplayMessage    = "message"
	DisplayToolCall   = "tool_call"
	DisplayToolOutput = "tool_output"
	DisplayReasoning  = "reasoning"
	DisplayHandoff    = "handoff"
	DisplayUnknown    = "unknown"
)

// MessageOutputItem is an assistant message produced by the model.
type MessageOutputItem struct {
	Agent *Agent
	Raw   TResponseOutputItem
	// Src is the item's provenance; the zero value means the model produced it.
	Src Source
}

// AgentRef implements RunItem.
func (i *MessageOutputItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *MessageOutputItem) ItemType() string { return "message_output" }
func (i *MessageOutputItem) isRunItem()       {}

// Source implements RunItem. A message is normally the model's, but the runner
// also synthesizes one for an error handler's fallback output; that path sets
// Src.
func (i *MessageOutputItem) Source() Source { return i.Src }

// Display implements RunItem.
func (i *MessageOutputItem) Display() ItemDisplay {
	return ItemDisplay{Kind: DisplayMessage, Text: i.Text()}
}

// ToInputItem implements RunItem.
func (i *MessageOutputItem) ToInputItem() (TResponseInputItem, error) {
	return outputItemToInput(i.Raw)
}

// Text returns the concatenated text content of the message.
func (i *MessageOutputItem) Text() string {
	return extractMessageText(i.Raw)
}

// ToolCallItem is a function tool call emitted by the model.
type ToolCallItem struct {
	Agent *Agent
	Raw   TResponseOutputItem
}

// AgentRef implements RunItem.
func (i *ToolCallItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *ToolCallItem) ItemType() string { return "tool_call" }
func (i *ToolCallItem) isRunItem()       {}

// Source implements RunItem. A tool call is always the model's decision.
func (i *ToolCallItem) Source() Source { return Source{} }

// Display implements RunItem.
func (i *ToolCallItem) Display() ItemDisplay {
	fc := i.FunctionCall()
	return ItemDisplay{
		Kind:      DisplayToolCall,
		CallID:    fc.CallID,
		ToolName:  fc.Name,
		Arguments: fc.Arguments,
	}
}

// ToInputItem implements RunItem.
func (i *ToolCallItem) ToInputItem() (TResponseInputItem, error) {
	return outputItemToInput(i.Raw)
}

// FunctionCall returns the underlying function tool call view.
func (i *ToolCallItem) FunctionCall() responses.ResponseFunctionToolCall {
	return i.Raw.AsFunctionCall()
}

// ToolCallOutputItem is the result of running a tool, formatted as a
// function_call_output input item.
type ToolCallOutputItem struct {
	Agent  *Agent
	Raw    TResponseInputItem
	Output any
	// Extra is SDK-only data the tool attached via ToolResult.Details. It is
	// not part of Raw, never reaches the model, and surfaces through
	// Display().Extra. It survives RunState serialization.
	Extra map[string]any
	// Renderer is the tool's ToolResult.Display hint.
	Renderer string
	// IsError marks a result that reports a tool failure.
	IsError bool
}

// AgentRef implements RunItem.
func (i *ToolCallOutputItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *ToolCallOutputItem) ItemType() string { return "tool_call_output" }
func (i *ToolCallOutputItem) isRunItem()       {}

// Source implements RunItem: a tool produced this, not the model.
func (i *ToolCallOutputItem) Source() Source { return Source{Type: SourceTool} }

// Display implements RunItem.
func (i *ToolCallOutputItem) Display() ItemDisplay {
	d := ItemDisplay{
		Kind:     DisplayToolOutput,
		Renderer: i.Renderer,
		Output:   stringifyToolOutput(i.Output),
		IsError:  i.IsError,
		Extra:    i.Extra,
	}
	if fco := i.Raw.OfFunctionCallOutput; fco != nil {
		d.CallID = fco.CallID
	}
	return d
}

// ToInputItem implements RunItem.
func (i *ToolCallOutputItem) ToInputItem() (TResponseInputItem, error) {
	return i.Raw, nil
}

// HandoffCallItem is the tool call by which the model requests a handoff.
type HandoffCallItem struct {
	Agent *Agent
	Raw   TResponseOutputItem
}

// AgentRef implements RunItem.
func (i *HandoffCallItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *HandoffCallItem) ItemType() string { return "handoff_call" }
func (i *HandoffCallItem) isRunItem()       {}

// Source implements RunItem: the model asked for the handoff.
func (i *HandoffCallItem) Source() Source { return Source{} }

// Display implements RunItem.
func (i *HandoffCallItem) Display() ItemDisplay {
	fc := i.Raw.AsFunctionCall()
	return ItemDisplay{
		Kind:      DisplayHandoff,
		CallID:    fc.CallID,
		ToolName:  fc.Name,
		Arguments: fc.Arguments,
	}
}

// ToInputItem implements RunItem.
func (i *HandoffCallItem) ToInputItem() (TResponseInputItem, error) {
	return outputItemToInput(i.Raw)
}

// HandoffOutputItem records the synthetic tool output acknowledging a handoff.
type HandoffOutputItem struct {
	Agent       *Agent
	Raw         TResponseInputItem
	SourceAgent *Agent
	TargetAgent *Agent
}

// AgentRef implements RunItem.
func (i *HandoffOutputItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *HandoffOutputItem) ItemType() string { return "handoff_output" }
func (i *HandoffOutputItem) isRunItem()       {}

// Source implements RunItem: the runner synthesized this acknowledgement.
func (i *HandoffOutputItem) Source() Source { return Source{Type: SourceHandoff} }

// Display implements RunItem.
func (i *HandoffOutputItem) Display() ItemDisplay {
	d := ItemDisplay{Kind: DisplayHandoff}
	if i.TargetAgent != nil {
		d.Text = i.TargetAgent.Name
	}
	if fco := i.Raw.OfFunctionCallOutput; fco != nil {
		d.CallID = fco.CallID
	}
	return d
}

// ToInputItem implements RunItem.
func (i *HandoffOutputItem) ToInputItem() (TResponseInputItem, error) {
	return i.Raw, nil
}

// ReasoningItem is a reasoning trace emitted by a reasoning model.
type ReasoningItem struct {
	Agent *Agent
	Raw   TResponseOutputItem
}

// AgentRef implements RunItem.
func (i *ReasoningItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *ReasoningItem) ItemType() string { return "reasoning" }
func (i *ReasoningItem) isRunItem()       {}

// Source implements RunItem.
func (i *ReasoningItem) Source() Source { return Source{} }

// Display implements RunItem.
func (i *ReasoningItem) Display() ItemDisplay {
	return ItemDisplay{Kind: DisplayReasoning, Text: i.Text()}
}

// ToInputItem implements RunItem.
func (i *ReasoningItem) ToInputItem() (TResponseInputItem, error) {
	return outputItemToInput(i.Raw)
}

// Text returns the item's thinking text: the standard summary parts, falling
// back to the content parts some Responses-compatible backends use for raw
// reasoning text. Encrypted-only reasoning yields "".
func (i *ReasoningItem) Text() string {
	r := i.Raw.AsReasoning()
	var b strings.Builder
	for _, s := range r.Summary {
		if s.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s.Text)
	}
	if b.Len() > 0 {
		return b.String()
	}
	for _, c := range r.Content {
		if c.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(c.Text)
	}
	return b.String()
}

// rawInputRunItem is a RunItem reconstituted from serialized state. It carries
// only the input-item form (and its original discriminator), which is all the
// runner needs to resume: history is rebuilt from input items.
type rawInputRunItem struct {
	Agent    *Agent
	RawInput TResponseInputItem
	Kind     string
	Src      Source
	Disp     ItemDisplay
}

func (i *rawInputRunItem) AgentRef() *Agent                         { return i.Agent }
func (i *rawInputRunItem) ItemType() string                         { return i.Kind }
func (i *rawInputRunItem) isRunItem()                               {}
func (i *rawInputRunItem) ToInputItem() (TResponseInputItem, error) { return i.RawInput, nil }
func (i *rawInputRunItem) Source() Source                           { return i.Src }
func (i *rawInputRunItem) Display() ItemDisplay                     { return i.Disp }

// ReasoningItemIDPolicy controls whether reasoning-item ids are preserved when
// run items are converted back into model input for a later turn. The default
// (ReasoningItemIDPreserve) keeps them; ReasoningItemIDOmit strips them, which is
// useful when replaying reasoning items whose server-side ids are no longer valid
// (e.g. store=false runs that rely on encrypted_content). It is the Go
// counterpart of Python's RunConfig.reasoning_item_id_policy.
type ReasoningItemIDPolicy int

const (
	// ReasoningItemIDPreserve keeps reasoning-item ids in model input (default).
	ReasoningItemIDPreserve ReasoningItemIDPolicy = iota
	// ReasoningItemIDOmit strips reasoning-item ids from model input.
	ReasoningItemIDOmit
)

// applyReasoningItemIDPolicy strips the id from reasoning input items when the
// policy is ReasoningItemIDOmit, mirroring Python's _without_reasoning_item_id
// (run_internal/items.py). It replaces the OfReasoning pointer with a modified
// copy so any RunItem or caller slice sharing the original param is unaffected.
//
// Note: the underlying openai-go reasoning param always serializes an "id" key,
// so an omitted id is sent as an empty string rather than dropped entirely; only
// the stale id value is removed.
func applyReasoningItemIDPolicy(items []TResponseInputItem, policy ReasoningItemIDPolicy) []TResponseInputItem {
	if policy != ReasoningItemIDOmit {
		return items
	}
	for i := range items {
		if r := items[i].OfReasoning; r != nil && r.ID != "" {
			cp := *r
			cp.ID = ""
			items[i].OfReasoning = &cp
		}
	}
	return items
}

// itemsToInputList converts a slice of RunItems into model input items.
func itemsToInputList(items []RunItem) ([]TResponseInputItem, error) {
	out := make([]TResponseInputItem, 0, len(items))
	for _, it := range items {
		in, err := it.ToInputItem()
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}

// extractMessageText pulls the concatenated output_text content from a message
// output item. It returns "" for non-message items.
func extractMessageText(item TResponseOutputItem) string {
	msg := item.AsMessage()
	var b strings.Builder
	for _, part := range msg.Content {
		if text := part.AsOutputText(); text.Text != "" {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// extractMessageRefusal pulls the concatenated refusal content from a message
// output item, or "" when the message carries none.
func extractMessageRefusal(item TResponseOutputItem) string {
	msg := item.AsMessage()
	var b strings.Builder
	for _, part := range msg.Content {
		if part.Type == "refusal" && part.Refusal != "" {
			b.WriteString(part.Refusal)
		}
	}
	return b.String()
}

// newFunctionCallOutputItem builds a ToolCallOutputItem for a function tool
// result. Structured/multimodal outputs (ToolOutputContent) become a content
// list so the model receives native text/image/file input; everything else is
// serialized to a string (JSON for non-string values).
func newFunctionCallOutputItem(agent *Agent, callID string, output any) *ToolCallOutputItem {
	raw, ok := toolOutputContentItem(callID, output)
	if !ok {
		raw = responses.ResponseInputItemParamOfFunctionCallOutput(callID, stringifyToolOutput(output))
	}
	return &ToolCallOutputItem{
		Agent:  agent,
		Raw:    raw,
		Output: output,
	}
}

// handoffOutputInput builds the function_call_output input item acknowledging a
// handoff, carrying the standard transfer message {"assistant": <agent name>}.
func handoffOutputInput(callID, targetAgentName string) TResponseInputItem {
	msg := fmt.Sprintf(`{"assistant":%q}`, targetAgentName)
	return responses.ResponseInputItemParamOfFunctionCallOutput(callID, msg)
}

// stringifyToolOutput renders a tool's return value as the string sent back to
// the model: strings pass through; everything else is JSON-encoded.
func stringifyToolOutput(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		// Unmarshalable values (NaN floats, channels, ...) degrade to fmt
		// rather than silently dropping the output.
		return fmt.Sprintf("%v", v)
	}
}

// UnknownOutputItem carries a model output item whose type this SDK does not
// model.
//
// It exists because the alternative was silently dropping it. The Responses API
// gains item types faster than any client tracks them, and a dropped item is
// not a missing feature — it is a corrupted conversation, because the next turn
// resends a history the model does not recognize as its own.
//
// The raw bytes go back on the wire unchanged, so a run that touches an unknown
// type behaves as if the SDK understood it. What is lost is only inspection:
// there is no typed accessor, and Display reports the type name.
type UnknownOutputItem struct {
	Agent *Agent
	Raw   TResponseOutputItem
}

// AgentRef implements RunItem.
func (i *UnknownOutputItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *UnknownOutputItem) ItemType() string { return "unknown" }
func (i *UnknownOutputItem) isRunItem()       {}

// Source implements RunItem: the model produced it.
func (i *UnknownOutputItem) Source() Source { return Source{} }

// Display implements RunItem. Text is the wire type name, which is all a
// renderer can honestly say about it.
func (i *UnknownOutputItem) Display() ItemDisplay {
	return ItemDisplay{Kind: DisplayUnknown, Text: i.Raw.Type}
}

// ToInputItem implements RunItem, returning the original bytes.
func (i *UnknownOutputItem) ToInputItem() (TResponseInputItem, error) {
	raw := i.Raw.RawJSON()
	if raw == "" {
		return TResponseInputItem{}, fmt.Errorf("unknown output item %q carries no raw JSON", i.Raw.Type)
	}
	return rawInputOverride(raw), nil
}
