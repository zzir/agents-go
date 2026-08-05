package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// The session layer speaks the same wire aliases as the runner. An alias is
// transparent — these ARE the agents-package types, redeclared so this package
// needs nothing from the runner.
type (
	// InputItem is a single item in the model input list, in OpenAI Responses
	// format.
	InputItem = responses.ResponseInputItemUnionParam
	// OutputItem is a single item produced by the model.
	OutputItem = responses.ResponseOutputItemUnion
)

// rawInputOverride wraps raw wire JSON as an input item that serializes back to
// exactly those bytes.
func rawInputOverride(raw string) InputItem {
	return param.Override[InputItem](json.RawMessage(raw))
}

// MarshalInputItem serializes an input item to JSON. It is the inverse of
// UnmarshalInputItem and is the encoding Session implementations should use.
func MarshalInputItem(item InputItem) ([]byte, error) {
	return json.Marshal(item)
}

// UnmarshalInputItem decodes an input item previously produced by
// MarshalInputItem. It works around two openai-go quirks: assistant messages
// with output content must decode into ResponseOutputMessageParam (the union
// decoder would match EasyInputMessageParam first and silently drop their
// content), and "easy" role messages serialize without a "type" discriminator,
// so the union decoder cannot auto-detect them. Session implementations should
// use it when reading stored items.
func UnmarshalInputItem(data []byte) (InputItem, error) {
	var item InputItem
	var probe struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	_ = json.Unmarshal(data, &probe)
	if probe.Type == "message" && probe.Role == "assistant" {
		var om responses.ResponseOutputMessageParam
		if err := json.Unmarshal(data, &om); err == nil && len(om.Content) > 0 {
			return InputItem{OfOutputMessage: &om}, nil
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
		return InputItem{OfMessage: &easy}, nil
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

// ItemText returns an input item's readable text, or "" for an item that has
// none (a tool call, a reasoning block).
//
// It exists because the Responses API accepts content as either a bare string
// or an array of parts, and a consumer rendering history would otherwise have
// to know that — and handle only the shape it happened to meet first.
func ItemText(item InputItem) string { return itemText(item) }

// itemText pulls the readable text out of an input item for its display.
func itemText(item InputItem) string {
	raw, err := MarshalInputItem(item)
	if err != nil {
		return ""
	}
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &probe) != nil || len(probe.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(probe.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(probe.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
