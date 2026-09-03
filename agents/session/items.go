package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// The session layer speaks the runner's wire aliases: these ARE the
// agents-package types, redeclared so this package needs nothing from the runner.
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
	// An easy input message ({"role","content"}) has no "type" discriminator:
	// decode it directly, requiring a role so arbitrary JSON is rejected.
	var easy responses.EasyInputMessageParam
	if err := json.Unmarshal(data, &easy); err == nil && easy.Role != "" {
		return InputItem{OfMessage: &easy}, nil
	}
	// A typed item the union does not know keeps its bytes: stored history can
	// outlive this SDK's type coverage. "type" is required so malformed JSON errors.
	if typ := probe.Type; typ != "" {
		return rawInputOverride(string(data)), nil
	}
	return item, fmt.Errorf("decoding input item: unrecognized item shape: %s", data)
}

// ItemText returns an input item's readable text, or "" for an item that has
// none (a tool call, a reasoning block). Content may be a bare string or an
// array of parts; both shapes are read.
func ItemText(item InputItem) string {
	raw, err := MarshalInputItem(item)
	if err != nil {
		return ""
	}
	return textFromRaw(raw)
}

// UserText returns the text of every role=="user" message in items, trimmed
// and joined by newlines — the string a user bubble shows; "" when there is none.
func UserText(items []InputItem) string {
	var parts []string
	for _, item := range items {
		raw, err := MarshalInputItem(item)
		if err != nil {
			continue
		}
		var probe struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &probe) != nil || probe.Role != "user" {
			continue
		}
		if txt := strings.TrimSpace(textFromRaw(raw)); txt != "" {
			parts = append(parts, txt)
		}
	}
	return strings.Join(parts, "\n")
}

// textFromRaw extracts a serialized item's "content" as either a bare string
// or an array of text parts, the two shapes the Responses API accepts.
func textFromRaw(raw []byte) string {
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
