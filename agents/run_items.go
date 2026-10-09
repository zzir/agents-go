package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/oaiitems"
)

// ItemKind classifies what a RunItem holds; a closed set whose strings are
// wire names (serialized in RunState).
type ItemKind string

const (
	// ItemMessage is an assistant message produced by the model.
	ItemMessage ItemKind = "message_output"
	// ItemToolCall is a function tool call emitted by the model.
	ItemToolCall ItemKind = "tool_call"
	// ItemToolCallOutput is the result of running a tool.
	ItemToolCallOutput ItemKind = "tool_call_output"
	// ItemHandoffCall is the tool call by which the model requests a handoff.
	ItemHandoffCall ItemKind = "handoff_call"
	// ItemHandoffOutput is the synthetic output acknowledging a handoff.
	ItemHandoffOutput ItemKind = "handoff_output"
	// ItemReasoning is a reasoning trace emitted by a reasoning model.
	ItemReasoning ItemKind = "reasoning"
	// ItemInjectedInput is caller input injected mid-run through RunControl —
	// see spec §2.11b.
	ItemInjectedInput ItemKind = "injected_input"
	// ItemUnknown carries a model output item this SDK does not model; the raw
	// bytes go back on the wire unchanged.
	ItemUnknown ItemKind = "unknown"
)

// RunItem is one thing that happened during a run: a model message, a tool
// call, a tool result, a handoff, a reasoning trace. Model-produced kinds
// carry Raw; runner-synthesized and rebuilt items carry RawInput — see spec §2.1b.
type RunItem struct {
	// Kind says what this item is; render an unknown kind as opaque.
	Kind ItemKind
	// Agent is the agent that produced the item.
	Agent *Agent
	// Source records who produced it. The zero value is the model.
	Source Source

	// Raw is the model's own output item; nil for synthesized and rebuilt items.
	Raw *OutputItem
	// RawInput is the item's input form, for synthesized and rebuilt items.
	RawInput *InputItem

	// Output is a tool's return value as the tool produced it (ItemToolCallOutput).
	Output any
	// Renderer is the tool's requested renderer, from ToolResult.Display.
	Renderer string
	// Title and Summary are the tool's display overrides; neither reaches the model.
	Title   string
	Summary string
	// IsError marks a tool result that reports a failure; the content still
	// reaches the model, and the tool-loop circuit breaker counts these.
	IsError bool
	// Extra is ToolResult.Details, surfaced through Display().Extra; never
	// reaches the model.
	Extra map[string]any
	// NestedUsage is what the tool spent on model calls of its own, kept apart
	// from the turn's usage — see spec §2.7f.
	NestedUsage *Usage

	// IsHandoff marks the tool-call event that wraps a handoff call — see spec §2.4.
	IsHandoff bool

	// HandoffFrom and HandoffTo name the agents a handoff moved between
	// (ItemHandoffOutput).
	HandoffFrom *Agent
	HandoffTo   *Agent

	// display is a rebuilt item's stored projection; its Raw is gone.
	display *ItemDisplay
}

// Display projects the item into the fields a renderer needs. It is a hint; a
// consumer must still be able to render from the item's own fields — see spec §2.1b.
func (i *RunItem) Display() ItemDisplay {
	if i.display != nil {
		return *i.display
	}
	switch i.Kind {
	case ItemMessage:
		return ItemDisplay{Kind: DisplayMessage, Text: i.Text()}
	case ItemReasoning:
		return ItemDisplay{Kind: DisplayReasoning, Text: i.Text()}
	case ItemToolCall:
		fc := i.FunctionCall()
		return ItemDisplay{Kind: DisplayToolCall, CallID: fc.CallID, ToolName: fc.Name, Arguments: fc.Arguments}
	case ItemHandoffCall:
		fc := i.FunctionCall()
		return ItemDisplay{Kind: DisplayHandoff, CallID: fc.CallID, ToolName: fc.Name, Arguments: fc.Arguments}
	case ItemToolCallOutput:
		return ItemDisplay{
			Kind:     DisplayToolOutput,
			Renderer: i.Renderer,
			Title:    i.Title,
			Summary:  i.Summary,
			Output:   stringifyToolOutput(i.Output),
			IsError:  i.IsError,
			Extra:    i.Extra,
			CallID:   i.CallID(),
		}
	case ItemHandoffOutput:
		d := ItemDisplay{Kind: DisplayHandoff, CallID: i.CallID()}
		if i.HandoffTo != nil {
			d.Text = i.HandoffTo.Name
		}
		return d
	default:
		// The wire type name is all a renderer can say about an unmodeled item.
		var name string
		if i.Raw != nil {
			name = i.Raw.Type
		}
		return ItemDisplay{Kind: DisplayUnknown, Text: name}
	}
}

// ToInputItem converts the item to a Responses API input item for the next turn.
func (i *RunItem) ToInputItem() (InputItem, error) {
	if i.RawInput != nil {
		return *i.RawInput, nil
	}
	if i.Raw == nil {
		return InputItem{}, fmt.Errorf("run item of kind %q carries neither a model item nor an input item", i.Kind)
	}
	return outputItemToInput(*i.Raw)
}

// Text returns the item's readable text: a message's content, a reasoning
// trace's summary parts (else its content parts); "" for other kinds.
func (i *RunItem) Text() string {
	if i.Raw == nil {
		// A rebuilt item's text survives only in its stored display.
		if i.display != nil {
			return i.display.Text
		}
		return ""
	}
	switch i.Kind {
	case ItemMessage:
		return extractMessageText(*i.Raw)
	case ItemReasoning:
		r := i.Raw.AsReasoning()
		var b strings.Builder
		for _, s := range r.Summary {
			appendTextPart(&b, s.Text)
		}
		if b.Len() > 0 {
			return b.String()
		}
		for _, c := range r.Content {
			appendTextPart(&b, c.Text)
		}
		return b.String()
	default:
		return ""
	}
}

// appendTextPart adds a non-empty part to the builder, blank-line separated.
func appendTextPart(b *strings.Builder, text string) {
	if text == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(text)
}

// refusal returns a message item's refusal content, or "".
func (i *RunItem) refusal() string {
	if i.Kind != ItemMessage || i.Raw == nil {
		return ""
	}
	return extractMessageRefusal(*i.Raw)
}

// FunctionCall returns the function tool call view (ItemToolCall,
// ItemHandoffCall); the zero value for other kinds.
func (i *RunItem) FunctionCall() FunctionToolCall {
	if i.Raw == nil {
		return FunctionToolCall{}
	}
	return i.Raw.AsFunctionCall()
}

// CallID ties a tool call to its output, read from whichever form the item carries.
func (i *RunItem) CallID() string {
	if i.RawInput != nil {
		if fco := i.RawInput.OfFunctionCallOutput; fco != nil {
			return fco.CallID.Value
		}
		return ""
	}
	if i.Raw != nil {
		return i.Raw.AsFunctionCall().CallID
	}
	return ""
}

// NewModelItem builds an item for something the model produced, for tests
// and code reconstructing a run's items.
func NewModelItem(kind ItemKind, agent *Agent, raw OutputItem) *RunItem {
	return &RunItem{Kind: kind, Agent: agent, Raw: &raw}
}

// ReasoningItemIDPolicy controls whether reasoning-item ids are kept when run
// items go back to the model; Omit serves store=false runs. Persisted in RunState.
type ReasoningItemIDPolicy int

const (
	// ReasoningItemIDPreserve keeps reasoning-item ids in model input (default).
	ReasoningItemIDPreserve ReasoningItemIDPolicy = iota
	// ReasoningItemIDOmit strips reasoning-item ids from model input.
	ReasoningItemIDOmit
)

// applyReasoningItemIDPolicy strips reasoning ids under ReasoningItemIDOmit on
// a copy of each param; openai-go always serializes "id", so it goes out empty.
func applyReasoningItemIDPolicy(items []InputItem, policy ReasoningItemIDPolicy) []InputItem {
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
func itemsToInputList(items []*RunItem) ([]InputItem, error) {
	out := make([]InputItem, 0, len(items))
	for _, it := range items {
		in, err := it.ToInputItem()
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}

// extractMessageText concatenates a message item's output_text parts; "" for
// other items.
func extractMessageText(item OutputItem) string {
	msg := item.AsMessage()
	var b strings.Builder
	for _, part := range msg.Content {
		if text := part.AsOutputText(); text.Text != "" {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// extractMessageRefusal concatenates a message item's refusal parts, or "".
func extractMessageRefusal(item OutputItem) string {
	msg := item.AsMessage()
	var b strings.Builder
	for _, part := range msg.Content {
		if part.Type == "refusal" && part.Refusal != "" {
			b.WriteString(part.Refusal)
		}
	}
	return b.String()
}

// newFunctionCallOutputItem builds a tool-result item: ToolOutputContent
// becomes a content list, everything else a string (JSON for non-strings).
func newFunctionCallOutputItem(agent *Agent, callID string, output any) *RunItem {
	raw, ok := toolOutputContentItem(callID, output)
	if !ok {
		raw = oaiitems.FunctionCallOutput(callID, responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(stringifyToolOutput(output))})
	}
	return &RunItem{
		Kind:     ItemToolCallOutput,
		Agent:    agent,
		Source:   Source{Type: SourceTool},
		RawInput: &raw,
		Output:   output,
	}
}

// newHandoffOutputItem builds the synthetic acknowledgement of a taken handoff.
func newHandoffOutputItem(agent, from, to *Agent, raw InputItem) *RunItem {
	return &RunItem{
		Kind:        ItemHandoffOutput,
		Agent:       agent,
		Source:      Source{Type: SourceHandoff},
		RawInput:    &raw,
		HandoffFrom: from,
		HandoffTo:   to,
	}
}

// handoffOutputInput builds the function_call_output acknowledging a handoff —
// see spec §2.4.
func handoffOutputInput(callID, targetAgentName string) InputItem {
	msg := fmt.Sprintf("{\"assistant\":%q}\n\nYou are now %q, handling this conversation directly.", targetAgentName, targetAgentName)
	return oaiitems.FunctionCallOutput(callID, responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(msg)})
}

// stringifyToolOutput renders a tool's return value as the string sent back to
// the model: strings pass through; everything else is JSON-encoded.
func stringifyToolOutput(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case nil:
		return ""
	case ToolOutputContent:
		return contentListJSON([]ToolOutputContent{v})
	case []ToolOutputContent:
		return contentListJSON(v)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		// An unmarshalable value (NaN, a channel) degrades to fmt.
		return fmt.Sprintf("%v", v)
	}
}

// contentListJSON renders a multimodal output as the Responses content list —
// see spec §2.7b.
func contentListJSON(parts []ToolOutputContent) string {
	wire := make([]responses.ResponseFunctionCallOutputItemUnionParam, 0, len(parts))
	for _, p := range parts {
		wire = append(wire, p.toContentParam())
	}
	b, err := json.Marshal(wire)
	if err != nil {
		return fmt.Sprintf("%v", parts)
	}
	return string(b)
}

// EntryFromRunItem builds a session entry from a run item with its provenance,
// display and agent; injected input takes no responseID.
func EntryFromRunItem(it *RunItem, responseID string) (session.Entry, error) {
	in, err := it.ToInputItem()
	if err != nil {
		return session.Entry{}, err
	}
	e, err := session.NewItemEntry(in, it.Source)
	if err != nil {
		return session.Entry{}, err
	}
	if it.Agent != nil {
		e.AgentName = it.Agent.Name
	}
	d := it.Display()
	e.Display = &d
	if it.Kind != ItemInjectedInput {
		e.ResponseID = responseID
	}
	if it.NestedUsage != nil {
		u := it.NestedUsage.Request()
		e.NestedUsage = &u
	}
	return e, nil
}
