package agents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

// ItemKind classifies what a RunItem holds. The set is closed: the runner
// produces these kinds and nothing else.
//
// The strings are wire names — they travel in a serialized RunState — so they
// are not renamed.
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
	// ItemInjectedInput is caller-supplied input injected mid-run through
	// RunControl, carried as an item so every downstream path — the next
	// turn's model input, the server-side delta cursor, the session write —
	// treats it exactly like the input the run started with.
	ItemInjectedInput ItemKind = "injected_input"
	// ItemUnknown carries a model output item whose type this SDK does not
	// model. The raw bytes go back on the wire unchanged.
	//
	// It exists because the alternative was silently dropping it. The Responses
	// API gains item types faster than any client tracks them, and a dropped
	// item is not a missing feature — it is a corrupted conversation, because
	// the next turn resends a history the model does not recognize as its own.
	// What is lost is only inspection: Display reports the wire type name.
	ItemUnknown ItemKind = "unknown"
)

// RunItem is one thing that happened during a run: a model message, a tool
// call, a tool result, a handoff, a reasoning trace.
//
// It is a struct with a Kind, not an interface with seven implementations. The
// set is closed — the runner produces these kinds and a caller cannot add one —
// and the members were near-identical: five of them held `{Agent, Raw}` and
// differed only in the tag they returned. Serialization had already arrived at
// this shape on its own: a serialized RunState stores `{type, agent, input,
// source, display}`, and rebuilding one needed an eighth, private item type
// whose whole job was to carry those fields back into the interface.
//
// Which fields carry meaning depends on Kind:
//
//	ItemMessage         Raw
//	ItemToolCall        Raw
//	ItemHandoffCall     Raw
//	ItemReasoning       Raw
//	ItemUnknown         Raw
//	ItemToolCallOutput  RawInput, Output, Renderer, IsError, Extra, NestedUsage
//	ItemHandoffOutput   RawInput, HandoffFrom, HandoffTo
//	ItemInjectedInput   RawInput
//
// An item rebuilt from a serialized RunState carries RawInput and a stored
// display instead of Raw, whatever its Kind: a resume replays history from
// input items, which is all it needs.
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
	// IsError marks a tool result that reports a failure. The content still
	// reaches the model, which is how a tool that failed usefully lets the
	// model recover; the tool-loop circuit breaker counts these.
	IsError bool
	// Extra is SDK-only data the tool attached via ToolResult.Details. It never
	// reaches the model and surfaces through Display().Extra.
	Extra map[string]any
	// NestedUsage is what the tool spent on model calls of its own — an
	// agent-as-tool's nested run, a summarization step. Nil when it called no
	// model.
	//
	// It is kept apart from the turn's own usage rather than added to it,
	// because the two answer different questions. "How big is this
	// conversation" is the parent's InputTokens; a nested run's tokens were
	// spent on a different conversation entirely, and folding them in would
	// make the context look larger than anything ever sent.
	NestedUsage *Usage

	// HandoffFrom and HandoffTo name the agents a handoff moved between
	// (ItemHandoffOutput).
	HandoffFrom *Agent
	HandoffTo   *Agent

	// display, when set, is the item's stored projection. Only a rebuilt item
	// has one: its Raw is gone, so the display cannot be derived again.
	display *ItemDisplay
}

// Display projects the item into the fields a renderer actually needs: the
// text, the tool call, the error flag. It is produced by the SDK, which knows
// the wire format, rather than by each consumer parsing it again — the version
// of that parsing living in agents-server was the source of a long tail of
// rendering bugs.
//
// It is a hint, not a replacement. A consumer that ignores Display entirely
// must still be able to render from the item's own fields; that is what keeps
// Display free to gain fields without breaking anyone.
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
// trace's thinking. It is "" for kinds that have none.
//
// For reasoning it reads the standard summary parts, falling back to the
// content parts some Responses-compatible backends use for raw reasoning text.
// Encrypted-only reasoning yields "".
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

// refusal returns a message item's refusal content, or "" — including for a
// rebuilt item, whose Raw is gone (a refusal fails the run before it is ever
// persisted, so a rebuilt message cannot be one).
func (i *RunItem) refusal() string {
	if i.Kind != ItemMessage || i.Raw == nil {
		return ""
	}
	return extractMessageRefusal(*i.Raw)
}

// FunctionCall returns the underlying function tool call view, for
// ItemToolCall and ItemHandoffCall. It is the zero value for other kinds.
func (i *RunItem) FunctionCall() responses.ResponseFunctionToolCall {
	if i.Raw == nil {
		return responses.ResponseFunctionToolCall{}
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

// NewModelItem builds an item for something the model produced: a message, a
// tool call, a handoff call, a reasoning trace, or an item type this build does
// not model.
//
// The runner builds these itself; this is for tests and for code that
// reconstructs a run's items from the outside.
func NewModelItem(kind ItemKind, agent *Agent, raw OutputItem) *RunItem {
	return &RunItem{Kind: kind, Agent: agent, Raw: &raw}
}

// ReasoningItemIDPolicy controls whether reasoning-item ids are preserved when
// run items are converted back into model input for a later turn. The default
// (ReasoningItemIDPreserve) keeps them; ReasoningItemIDOmit strips them, which is
// useful when replaying reasoning items whose server-side ids are no longer valid
// (e.g. store=false runs that rely on encrypted_content). It is persisted across
// interruptions in RunState.
type ReasoningItemIDPolicy int

const (
	// ReasoningItemIDPreserve keeps reasoning-item ids in model input (default).
	ReasoningItemIDPreserve ReasoningItemIDPolicy = iota
	// ReasoningItemIDOmit strips reasoning-item ids from model input.
	ReasoningItemIDOmit
)

// applyReasoningItemIDPolicy strips the id from reasoning input items when the
// policy is ReasoningItemIDOmit. It replaces the OfReasoning pointer with a
// modified copy so any RunItem or caller slice sharing the original param is
// unaffected.
//
// Note: the underlying openai-go reasoning param always serializes an "id" key,
// so an omitted id is sent as an empty string rather than dropped entirely; only
// the stale id value is removed.
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

// newFunctionCallOutputItem builds a tool-result item. Structured/multimodal
// outputs (ToolOutputContent) become a content list so the model receives native
// text/image/file input; everything else is serialized to a string (JSON for
// non-string values).
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

// handoffOutputInput builds the function_call_output input item acknowledging a
// handoff, carrying the standard transfer message {"assistant": <agent name>}.
func handoffOutputInput(callID, targetAgentName string) InputItem {
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

// EntryFromRunItem builds a session entry from a run item, carrying its
// provenance, display and owning agent.
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
	e.ResponseID = responseID
	if it.NestedUsage != nil {
		u := it.NestedUsage.Request()
		e.NestedUsage = &u
	}
	return e, nil
}
