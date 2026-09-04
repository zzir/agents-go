package store

import (
	"encoding/json"
	"strings"
)

// adaptForeignItemJSON adapts an item produced by a different model for
// replay: reasoning items are dropped (nil), provider-assigned item ids stripped.
func adaptForeignItemJSON(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	var typ string
	if t, ok := m["type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	if typ == "reasoning" {
		return nil
	}
	if _, ok := m["id"]; !ok {
		return raw
	}
	delete(m, "id")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// NormalizeItemJSON rewrites a stored item for strict Responses-API backends
// that require message `content` to be an array: a bare string is wrapped in
// a one-part input_text array, and a literal `"content": null` is dropped.
func NormalizeItemJSON(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	c, ok := m["content"]
	if !ok {
		return raw
	}
	if string(c) == "null" {
		delete(m, "content")
		out, err := json.Marshal(m)
		if err != nil {
			return raw
		}
		return out
	}
	var text string
	if json.Unmarshal(c, &text) != nil {
		return raw // already an array/object — leave as-is
	}
	var role string
	if r, ok := m["role"]; ok {
		_ = json.Unmarshal(r, &role)
	}
	if role != "user" && role != "system" && role != "developer" {
		return raw
	}
	parts, err := json.Marshal([]map[string]string{{"type": "input_text", "text": text}})
	if err != nil {
		return raw
	}
	m["content"] = parts
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// itemTextJSON pulls the readable text out of stored item JSON, from the raw
// bytes: a backend can type a content part "text" instead of "output_text".
func itemTextJSON(raw []byte) string {
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &probe) != nil || len(probe.Content) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(probe.Content, &text) == nil {
		return text
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
