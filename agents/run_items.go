package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

// ItemKind classifies what a RunItem holds. The set is closed: the runner
// produces these kinds and nothing else. The strings are wire names (they
// travel in a serialized RunState), so they are not renamed.
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
	// ItemInjectedInput is caller input injected mid-run through RunControl,
	// carried as an item so every downstream path treats it as input (§2.11b).
	ItemInjectedInput ItemKind = "injected_input"
	// ItemUnknown carries a model output item this SDK does not model; the raw
	// bytes go back on the wire unchanged, and Display reports the type name.
	ItemUnknown ItemKind = "unknown"
)

// RunItem is one thing that happened during a run: a model message, a tool
// call, a tool result, a handoff, a reasoning trace. Which fields carry
// meaning depends on Kind:
//
//	ItemMessage         Raw
//	ItemToolCall        Raw, IsHandoff
//	ItemHandoffCall     Raw
//	ItemReasoning       Raw
//	ItemUnknown         Raw
//	ItemToolCallOutput  RawInput, Output, Renderer, Title, Summary, IsError, Extra, NestedUsage
//	ItemHandoffOutput   RawInput, HandoffFrom, HandoffTo
//	ItemInjectedInput   RawInput
//
// An item rebuilt from a serialized RunState carries RawInput and a stored
// display instead of Raw, whatever its Kind.
type RunItem struct {
	// Kind says what this item is. A consumer that meets a kind it does not
	// know should render it as opaque rather than fail.
	Kind ItemKind
	// Agent is the agent that produced the item.
	Agent *Agent
	// Source records who produced it. The zero value is the model.
	Source Source

	// Raw is the model's own output item, for the kinds the model produced.
	// Nil for runner-synthesized kinds and for items rebuilt from a RunState.
	Raw *OutputItem
	// RawInput is the item's input form, for the kinds the runner synthesized
	// (a tool result, a handoff acknowledgement) and for rebuilt items.
	RawInput *InputItem

	// Output is a tool's return value as the tool produced it, before it was
	// rendered for the model (ItemToolCallOutput).
	Output any
	// Renderer is the tool's requested renderer, from ToolResult.Display.
	Renderer string
	// Title and Summary are the tool's display overrides (ToolResult.Title /
	// Summary): a card heading and a one-line account. Empty falls back;
	// neither reaches the model.
	Title   string
	Summary string
	// IsError marks a tool result that reports a failure. The content still
	// reaches the model, which is how a tool that failed usefully lets the
	// model recover; the tool-loop circuit breaker counts these.
	IsError bool
	// Extra is SDK-only data the tool attached via ToolResult.Details. It never
	// reaches the model and surfaces through Display().Extra.
	Extra map[string]any
	// NestedUsage is what the tool spent on model calls of its own; nil when
	// it called none. Kept apart from the turn's usage, not added to it
	// (spec §2.7f).
	NestedUsage *Usage

	// IsHandoff marks the tool_called event that wraps a handoff call: the
	// same call also arrives as handoff_requested, and the flag lets a consumer
	// drop or badge the wrapped form without knowing every handoff tool name.
	IsHandoff bool

	// HandoffFrom and HandoffTo name the agents a handoff moved between
	// (ItemHandoffOutput).
	HandoffFrom *Agent
	HandoffTo   *Agent

	// display, when set, is the item's stored projection. Only a rebuilt item
	// has one: its Raw is gone, so the display cannot be derived again.
	display *ItemDisplay
}

// Display projects the item into the fields a renderer needs: the text, the
// tool call, the error flag — produced by the SDK, which knows the wire format.
// It is a hint: a consumer must still be able to render from the item's own
// fields, which keeps Display free to gain fields.
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
		// The wire type name is all a renderer can honestly say about an item
		// this build does not model.
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
// trace's thinking; "" for kinds that have none. Reasoning reads the summary
// parts, falling back to the content parts some backends use for raw
// reasoning text; encrypted-only reasoning yields "".
func (i *RunItem) Text() string {
	if i.Raw == nil {
		// A rebuilt item has no model item left; its stored display is the only
		// place its text survives.
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

// appendTextPart adds a non-empty part to a reasoning text builder, separated
// by a blank line from what came before.
func appendTextPart(b *strings.Builder, text string) {
	if text == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(text)
}

// refusal returns a message item's refusal content, or "" — a rebuilt item
// included (a refusal fails the run before it is ever persisted).
func (i *RunItem) refusal() string {
	if i.Kind != ItemMessage || i.Raw == nil {
		return ""
	}
	return extractMessageRefusal(*i.Raw)
}

// FunctionCall returns the underlying function tool call view, for
// ItemToolCall and ItemHandoffCall. It is the zero value for other kinds.
func (i *RunItem) FunctionCall() FunctionToolCall {
	if i.Raw == nil {
		return FunctionToolCall{}
	}
	return i.Raw.AsFunctionCall()
}

// CallID ties a tool call to its output, read from whichever form the item
// carries.
func (i *RunItem) CallID() string {
	if i.RawInput != nil {
		if fco := i.RawInput.OfFunctionCallOutput; fco != nil {
			return fco.CallID
		}
		return ""
	}
	if i.Raw != nil {
		return i.Raw.AsFunctionCall().CallID
	}
	return ""
}

// NewModelItem builds an item for something the model produced. The runner
// builds these itself; this is for tests and for code that reconstructs a
// run's items from the outside.
func NewModelItem(kind ItemKind, agent *Agent, raw OutputItem) *RunItem {
	return &RunItem{Kind: kind, Agent: agent, Raw: &raw}
}

// ReasoningItemIDPolicy controls whether reasoning-item ids are kept when run
// items are converted back into model input. The default preserves them;
// ReasoningItemIDOmit strips them, for replaying reasoning whose server-side
// ids are no longer valid (store=false runs). Persisted in RunState.
type ReasoningItemIDPolicy int

const (
	// ReasoningItemIDPreserve keeps reasoning-item ids in model input (default).
	ReasoningItemIDPreserve ReasoningItemIDPolicy = iota
	// ReasoningItemIDOmit strips reasoning-item ids from model input.
	ReasoningItemIDOmit
)

// applyReasoningItemIDPolicy strips reasoning ids under ReasoningItemIDOmit,
// on a copy of each param. openai-go always serializes "id", so it is sent empty.
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

// extractMessageText pulls the concatenated output_text content from a message
// output item. It returns "" for non-message items.
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

// extractMessageRefusal pulls the concatenated refusal content from a message
// output item, or "" when the message carries none.
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
		raw = responses.ResponseInputItemParamOfFunctionCallOutput(callID, stringifyToolOutput(output))
	}
	return &RunItem{
		Kind:     ItemToolCallOutput,
		Agent:    agent,
		Source:   Source{Type: SourceTool},
		RawInput: &raw,
		Output:   output,
	}
}

// newHandoffOutputItem builds the synthetic acknowledgement recorded when a
// handoff is taken.
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

// handoffOutputInput builds the function_call_output acknowledging a handoff:
// the transfer marker plus an identity line for the target — spec §2.4.
func handoffOutputInput(callID, targetAgentName string) InputItem {
	msg := fmt.Sprintf("{\"assistant\":%q}\n\nYou are now %q, handling this conversation directly.", targetAgentName, targetAgentName)
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
	case ToolOutputContent:
		return contentListJSON([]ToolOutputContent{v})
	case []ToolOutputContent:
		return contentListJSON(v)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		// Unmarshalable values (NaN floats, channels, ...) degrade to fmt
		// rather than silently dropping the output.
		return fmt.Sprintf("%v", v)
	}
}

// contentListJSON renders a multimodal output as the Responses content list
// the model receives, not this package's Go types (spec §2.7b).
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

// EntryFromRunItem builds a session entry from a run item, carrying its
// provenance, display and owning agent. responseID is the response the item
// came from; injected input came from the caller and gets none.
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
