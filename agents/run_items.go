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
	isRunItem()
}

// MessageOutputItem is an assistant message produced by the model.
type MessageOutputItem struct {
	Agent *Agent
	Raw   TResponseOutputItem
}

// AgentRef implements RunItem.
func (i *MessageOutputItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *MessageOutputItem) ItemType() string { return "message_output" }
func (i *MessageOutputItem) isRunItem()       {}

// ToInputItem implements RunItem.
func (i *MessageOutputItem) ToInputItem() (TResponseInputItem, error) {
	return outputItemToInput(i.Raw)
}

// Text returns the concatenated text content of the message.
func (i *MessageOutputItem) Text() string {
	return extractMessageText(i.Raw)
}

// ToolCallItem is a function (or hosted) tool call emitted by the model.
type ToolCallItem struct {
	Agent *Agent
	Raw   TResponseOutputItem
}

// AgentRef implements RunItem.
func (i *ToolCallItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *ToolCallItem) ItemType() string { return "tool_call" }
func (i *ToolCallItem) isRunItem()       {}

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
}

// AgentRef implements RunItem.
func (i *ToolCallOutputItem) AgentRef() *Agent { return i.Agent }

// ItemType implements RunItem.
func (i *ToolCallOutputItem) ItemType() string { return "tool_call_output" }
func (i *ToolCallOutputItem) isRunItem()       {}

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

// ToInputItem implements RunItem.
func (i *ReasoningItem) ToInputItem() (TResponseInputItem, error) {
	return outputItemToInput(i.Raw)
}

// rawInputRunItem is a RunItem reconstituted from serialized state. It carries
// only the input-item form (and its original discriminator), which is all the
// runner needs to resume: history is rebuilt from input items.
type rawInputRunItem struct {
	Agent    *Agent
	RawInput TResponseInputItem
	Kind     string
}

func (i *rawInputRunItem) AgentRef() *Agent                         { return i.Agent }
func (i *rawInputRunItem) ItemType() string                         { return i.Kind }
func (i *rawInputRunItem) isRunItem()                               {}
func (i *rawInputRunItem) ToInputItem() (TResponseInputItem, error) { return i.RawInput, nil }

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
// result, serializing non-string outputs to JSON.
func newFunctionCallOutputItem(agent *Agent, callID string, output any) *ToolCallOutputItem {
	return &ToolCallOutputItem{
		Agent:  agent,
		Raw:    responses.ResponseInputItemParamOfFunctionCallOutput(callID, stringifyToolOutput(output)),
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
