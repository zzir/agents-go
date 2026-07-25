package agents

import (
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// The agents SDK speaks the OpenAI Responses API item format as its lingua
// franca, exactly as the Python SDK does. These aliases give the rest of the
// package stable, intention-revealing names for the underlying openai-go types.
type (
	// TResponseInputItem is a single item in the model input list, in OpenAI
	// Responses format. A conversation history is a slice of these.
	TResponseInputItem = responses.ResponseInputItemUnionParam

	// TResponseOutputItem is a single item produced by the model, in OpenAI
	// Responses format.
	TResponseOutputItem = responses.ResponseOutputItemUnion

	// TResponseStreamEvent is a single streaming event from the Responses API.
	TResponseStreamEvent = responses.ResponseStreamEventUnion
)

// ModelResponse is the full result of a single model call.
//
// It is the Go counterpart of the Python SDK's ModelResponse dataclass.
type ModelResponse struct {
	// Output is the items produced by the model (messages, tool calls, etc).
	Output []TResponseOutputItem
	// Usage is the token usage for this request.
	Usage *Usage
	// ResponseID is the provider response identifier, used to chain calls via
	// previous_response_id on the Responses API.
	ResponseID string
	// RequestID is the provider request identifier from the transport response
	// headers (e.g. OpenAI's x-request-id), useful for support/debugging. Empty
	// when the backend does not supply one.
	RequestID string
}

// ToInputItems converts the model output items into input items suitable for the
// next model call. Output and input share the same wire format, so the
// conversion round-trips through JSON.
func (m *ModelResponse) ToInputItems() ([]TResponseInputItem, error) {
	return OutputToInput(m.Output)
}

// OutputToInput converts a slice of model output items into input items by
// re-encoding each item's wire JSON into the input union. This mirrors the
// per-item to_input_item() conversions in the Python SDK.
func OutputToInput(out []TResponseOutputItem) ([]TResponseInputItem, error) {
	items := make([]TResponseInputItem, 0, len(out))
	for i := range out {
		in, err := outputItemToInput(out[i])
		if err != nil {
			return nil, fmt.Errorf("converting output item %d to input: %w", i, err)
		}
		items = append(items, in)
	}
	return items, nil
}

// knownOutputTypes are the output item types the SDK models with a typed
// variant. Anything else round-trips through param.Override instead — see
// outputItemToInput.
//
// It is the same set processModelResponse switches on; keeping one list means a
// type cannot become "known" to one and unknown to the other.
var knownOutputTypes = map[string]bool{
	"message":              true,
	"reasoning":            true,
	"function_call":        true,
	"function_call_output": true,
}

func outputItemToInput(out TResponseOutputItem) (TResponseInputItem, error) {
	var in TResponseInputItem
	raw := out.RawJSON()
	if raw == "" {
		return in, fmt.Errorf("output item has no raw JSON to convert")
	}
	// Assistant messages must be converted explicitly: the input union decoder
	// matches "type":"message" against EasyInputMessageParam first, whose content
	// union cannot represent output_text/refusal parts, silently dropping them.
	if out.Type == "message" {
		p := out.AsMessage().ToParam()
		return TResponseInputItem{OfOutputMessage: &p}, nil
	}
	// A type we do not model goes back on the wire byte for byte. Decoding it
	// into the typed union would drop every field the union does not know, and
	// the API added the type for a reason — a client that silently discards it
	// corrupts the conversation rather than merely ignoring a feature.
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
func rawInputOverride(raw string) TResponseInputItem {
	return param.Override[TResponseInputItem](json.RawMessage(raw))
}

// MarshalInputItem serializes an input item to JSON. It is the inverse of
// UnmarshalInputItem and is the encoding Session implementations should use.
func MarshalInputItem(item TResponseInputItem) ([]byte, error) {
	return json.Marshal(item)
}

// UnmarshalInputItem decodes an input item previously produced by
// MarshalInputItem. It works around two openai-go quirks: assistant messages
// with output content must decode into ResponseOutputMessageParam (the union
// decoder would match EasyInputMessageParam first and silently drop their
// content), and "easy" role messages serialize without a "type" discriminator,
// so the union decoder cannot auto-detect them. Session implementations should
// use it when reading stored items.
func UnmarshalInputItem(data []byte) (TResponseInputItem, error) {
	var item TResponseInputItem
	var probe struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	_ = json.Unmarshal(data, &probe)
	if probe.Type == "message" && probe.Role == "assistant" {
		var om responses.ResponseOutputMessageParam
		if err := json.Unmarshal(data, &om); err == nil && len(om.Content) > 0 {
			return TResponseInputItem{OfOutputMessage: &om}, nil
		}
	}
	if err := json.Unmarshal(data, &item); err == nil {
		return item, nil
	}
	// Fallback: an easy input message ({"role":..., "content":...}) lacks a
	// "type" discriminator, so decode it directly. Require a role so arbitrary
	// JSON objects are rejected instead of becoming empty messages.
	var easy responses.EasyInputMessageParam
	if err := json.Unmarshal(data, &easy); err == nil && easy.Role != "" {
		return TResponseInputItem{OfMessage: &easy}, nil
	}
	// A typed item the union does not know: keep the bytes rather than reject
	// them. Stored history can outlive this SDK's type coverage — a session
	// written by a newer build, or one holding an item type added after it was
	// written — and refusing to read it back would make the whole conversation
	// unloadable over one item.
	//
	// A "type" is required, so genuinely malformed JSON still errors instead of
	// being smuggled through as an opaque blob.
	if typ := probe.Type; typ != "" {
		return rawInputOverride(string(data)), nil
	}
	return item, fmt.Errorf("decoding input item: unrecognized item shape: %s", data)
}

// InputItemsFromText builds a single-message input list from a user string. It
// is a convenience for the common case of running an agent on plain text.
func InputItemsFromText(text string) []TResponseInputItem {
	return []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser),
	}
}

// InputItemsFromSystemText builds a single system message.
//
// It is what the runtime uses to say something itself — a compaction summary,
// a folded record of tool calls. Attributing such content to the user or the
// assistant would put words in someone's mouth, and the model would treat it as
// something that was actually said.
func InputItemsFromSystemText(text string) []TResponseInputItem {
	return []TResponseInputItem{
		responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleSystem),
	}
}
