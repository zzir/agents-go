package agents

import (
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// The Responses API item format is the SDK's only canonical format; these
// aliases name the openai-go types — see decisions §5.10.
type (
	// InputItem is a single item in the model input list; a history is a slice
	// of these.
	InputItem = responses.ResponseInputItemUnionParam

	// OutputItem is a single item produced by the model.
	OutputItem = responses.ResponseOutputItemUnion

	// FunctionToolCall is the function-call view of an output item: tool name,
	// JSON arguments and the call ID that ties it to its output.
	FunctionToolCall = responses.ResponseFunctionToolCall

	// ResponseStreamEvent is a single streaming event from the Responses API.
	ResponseStreamEvent = responses.ResponseStreamEventUnion
)

// ModelResponse is the full result of a single model call.
type ModelResponse struct {
	// Output is the items produced by the model (messages, tool calls, etc).
	Output []OutputItem
	// Usage is the token usage for this request.
	Usage *Usage
	// ResponseID is the provider response id, chained via previous_response_id.
	ResponseID string
	// RequestID is the transport request id (OpenAI's x-request-id); empty
	// when the backend supplies none.
	RequestID string
	// Model is the model the provider reports answering with; empty when it
	// does not say.
	Model string
	// Status is the provider's verdict ("completed", "incomplete", …) and
	// IncompleteReason says why; an incomplete response still carries items.
	// See Truncated.
	Status           string
	IncompleteReason string
}

// Truncated reports whether the response was cut off at the output-token
// limit; its tail, a tool call's arguments included, may be half-formed.
func (m *ModelResponse) Truncated() bool {
	return m != nil && m.Status == "incomplete" && m.IncompleteReason == "max_output_tokens"
}

// OutputToInput converts a slice of model output items into input items by
// re-encoding each item's wire JSON into the input union.
func OutputToInput(out []OutputItem) ([]InputItem, error) {
	items := make([]InputItem, 0, len(out))
	for i := range out {
		in, err := outputItemToInput(out[i])
		if err != nil {
			return nil, fmt.Errorf("converting output item %d to input: %w", i, err)
		}
		items = append(items, in)
	}
	return items, nil
}

// knownOutputTypes are the output item types with a typed variant; anything
// else round-trips through param.Override. The set processModelResponse switches on.
var knownOutputTypes = map[string]bool{
	"message":              true,
	"reasoning":            true,
	"function_call":        true,
	"function_call_output": true,
}

func outputItemToInput(out OutputItem) (InputItem, error) {
	var in InputItem
	raw := out.RawJSON()
	if raw == "" {
		return in, fmt.Errorf("output item has no raw JSON to convert")
	}
	// An assistant message is converted explicitly: the input union decoder would
	// match EasyInputMessageParam and drop output_text/refusal parts. Logprobs
	// stay behind — see spec §2.1b.
	if out.Type == "message" {
		cleaned, _ := stripMessageLogprobs([]byte(raw))
		p := param.Override[responses.ResponseOutputMessageParam](json.RawMessage(cleaned))
		return InputItem{OfOutputMessage: &p}, nil
	}
	// An unmodeled type goes back on the wire byte for byte — see spec §2.1b.
	if !knownOutputTypes[out.Type] {
		return rawInputOverride(raw), nil
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return in, err
	}
	return in, nil
}

// rawInputOverride wraps raw wire JSON as an input item that serializes back to
// exactly those bytes.
func rawInputOverride(raw string) InputItem {
	return param.Override[InputItem](json.RawMessage(raw))
}

// InputItemsFromText builds a single user message input list from text.
func InputItemsFromText(text string) []InputItem {
	return []InputItem{
		responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser),
	}
}

// InputItemsFromAssistantText builds a single assistant message, for seeding
// history the SDK did not produce.
func InputItemsFromAssistantText(text string) []InputItem {
	return []InputItem{
		responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleAssistant),
	}
}

// InputItemsFromSystemText builds a single system message (a compaction
// summary, a folded record of tool calls).
func InputItemsFromSystemText(text string) []InputItem {
	return []InputItem{
		responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleSystem),
	}
}
