package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// The wire types, aliased here so this package needs nothing from the runner.
type (
	// InputItem is a single item in the model input list, in Responses format.
	InputItem = responses.ResponseInputItemUnionParam
	// OutputItem is a single item produced by the model.
	OutputItem = responses.ResponseOutputItemUnion
)

// rawInputOverride wraps raw wire JSON as an input item that serializes back to
// exactly those bytes.
func rawInputOverride(raw string) InputItem {
	return param.Override[InputItem](json.RawMessage(raw))
}

// MarshalInputItem serializes an input item to JSON; UnmarshalInputItem is its inverse.
func MarshalInputItem(item InputItem) ([]byte, error) {
	return json.Marshal(item)
}

// UnmarshalInputItem decodes an item MarshalInputItem produced, around two
// openai-go quirks: an assistant message with output content decodes as
// ResponseOutputMessageParam (the union would drop its content), and an "easy"
// role message has no "type" discriminator for the union to detect it by.
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
	// No "type" discriminator: decode directly, requiring a role so arbitrary
	// JSON is rejected.
	var easy responses.EasyInputMessageParam
	if err := json.Unmarshal(data, &easy); err == nil && easy.Role != "" {
		return InputItem{OfMessage: &easy}, nil
	}
	// An unknown typed item keeps its bytes (history outlives this build's types);
	// "type" is required so malformed JSON errors.
	if typ := probe.Type; typ != "" {
		return rawInputOverride(string(data)), nil
	}
	return item, fmt.Errorf("decoding input item: unrecognized item shape: %s", data)
}

// ItemText returns an input item's readable text, "" for one with none (a tool
// call, a reasoning block); content may be a bare string or parts.
func ItemText(item InputItem) string {
	raw, err := MarshalInputItem(item)
	if err != nil {
		return ""
	}
	return textFromRaw(raw)
}

// UserText returns the text of every role=="user" message in items, trimmed
// and joined by newlines; "" when there is none.
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

// ItemRole classifies an item's wire JSON: a message's role, "tool" for a
// function call or its output, "" for anything else.
func ItemRole(raw json.RawMessage) string {
	p := ProbeItem(raw)
	switch p.Type {
	case "function_call", "function_call_output":
		return "tool"
	case "reasoning":
		return ""
	}
	return p.Role
}

// RenderItem is an item's readable text: a message's content, a function call
// as name(arguments), an output's text; "" for an item with nothing to say.
func RenderItem(raw json.RawMessage) string {
	p := ProbeItem(raw)
	switch p.Type {
	case "function_call":
		if p.Name == "" {
			return ""
		}
		return p.Name + "(" + p.Args + ")"
	case "function_call_output":
		return JSONText(p.Output)
	case "reasoning":
		return ""
	}
	if p.Role == "" && p.Type != "message" {
		return ""
	}
	return textFromRaw(raw)
}

// JSONText unwraps a JSON string, the raw JSON for a structured payload; "" for
// nothing.
func JSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// textFromRaw extracts a serialized item's "content", a bare string or an array
// of text parts.
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
