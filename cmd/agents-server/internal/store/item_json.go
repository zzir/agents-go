package store

import (
	"encoding/json"
	"strings"
)

// This file is what survived the messages table: two functions about the WIRE
// FORMAT of an item, which the entry model does not change.
//
// Everything else it held — the row shape, the display derivation, the adapter
// bridging rows to session.Storage — existed to reconstruct at read time
// what the SDK already knew at write time. See entry_store.go.

// adaptForeignItemJSON adapts an item produced by a different model for
// replay: reasoning items are dropped entirely (their shape and ids are
// provider-specific and rejected by other backends), and provider-assigned
// item ids are stripped so the target backend does not try to resolve them.
// Returns nil when the item must be skipped.
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

// NormalizeItemJSON rewrites a stored item for replay compatibility with
// strict Responses-API backends that require message `content` to always be an
// array: user/system/developer messages stored with bare-string content (the
// shape the SDK writes for plain-text input) get the string wrapped in a
// one-part input_text array, and a literal `"content": null` (seen on some
// backends' reasoning items) is dropped entirely. Items already in array form
// pass through untouched.
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

// itemTextJSON pulls the readable text out of stored item JSON.
//
// It reads the raw bytes rather than the SDK's typed union on purpose: a
// Responses-compatible backend (vLLM and friends) can type a content part
// "text" instead of "output_text", and a shape the union does not model comes
// back from a round-trip empty. The stored JSON still has the text.
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
