package providers

import (
	"testing"
)

func TestSanitizeChatGPTInput_SkipsItemReference(t *testing.T) {
	input := []any{
		map[string]any{"type": "item_reference", "id": "ref_123"},
		map[string]any{"type": "message", "role": "user", "content": "hi"},
	}
	out := sanitizeChatGPTInput(input)
	if len(out) != 1 {
		t.Fatalf("expected 1 item, got %d", len(out))
	}
}

func TestSanitizeChatGPTInput_Message(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hello", "annotations": []any{}}},
			"id": "msg_123", "status": "completed", "phase": "thinking",
		},
	}
	out := sanitizeChatGPTInput(input)
	m := out[0].(map[string]any)
	for _, banned := range []string{"id", "status", "phase"} {
		if _, ok := m[banned]; ok {
			t.Errorf("message should not contain %q", banned)
		}
	}
	for _, required := range []string{"type", "role", "content"} {
		if _, ok := m[required]; !ok {
			t.Errorf("message missing required field %q", required)
		}
	}
}

func TestSanitizeChatGPTInput_FunctionCall(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "function_call", "call_id": "call_1", "name": "exec_command",
			"arguments": `{"cmd":"ls"}`, "id": "fc_1", "status": "completed",
		},
	}
	out := sanitizeChatGPTInput(input)
	m := out[0].(map[string]any)
	for _, banned := range []string{"id", "status"} {
		if _, ok := m[banned]; ok {
			t.Errorf("function_call should not contain %q", banned)
		}
	}
	for _, required := range []string{"type", "call_id", "name", "arguments"} {
		if _, ok := m[required]; !ok {
			t.Errorf("function_call missing required field %q", required)
		}
	}
}

func TestSanitizeChatGPTInput_FunctionCallOutput(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "function_call_output", "call_id": "call_1", "output": "ok",
			"id": "fco_1", "status": "completed",
		},
	}
	out := sanitizeChatGPTInput(input)
	m := out[0].(map[string]any)
	if _, ok := m["id"]; ok {
		t.Error("function_call_output should not contain id")
	}
	if _, ok := m["status"]; ok {
		t.Error("function_call_output should not contain status")
	}
	if m["output"] != "ok" {
		t.Error("function_call_output should preserve output")
	}
}

func TestSanitizeChatGPTInput_Reasoning(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "reasoning", "content": []any{map[string]any{"type": "text", "text": "thinking..."}},
			"summary":           []any{map[string]any{"type": "text", "text": "summary"}},
			"encrypted_content": "enc_data",
			"id":                "rs_1", "status": "completed",
		},
	}
	out := sanitizeChatGPTInput(input)
	m := out[0].(map[string]any)
	for _, banned := range []string{"id", "status"} {
		if _, ok := m[banned]; ok {
			t.Errorf("reasoning should not contain %q", banned)
		}
	}
	for _, required := range []string{"type", "summary", "encrypted_content"} {
		if _, ok := m[required]; !ok {
			t.Errorf("reasoning missing required field %q", required)
		}
	}
	// The codex backend caps reasoning content at length 0.
	if c, ok := m["content"].([]any); !ok || len(c) != 0 {
		t.Errorf("reasoning content should be an empty array, got %v", m["content"])
	}
}

// A reasoning item replayed from another provider carries reasoning_text content
// (rejected: "content array too long, maximum length 0") and a foreign signature
// (rejected: "invalid_encrypted_content"). The codex backend can use neither, so
// the whole reasoning item is dropped and the surrounding turn is left intact.
func TestSanitizeChatGPTInput_ReasoningCrossProviderDropped(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "hello"},
		map[string]any{
			"type":              "reasoning",
			"summary":           []any{},
			"encrypted_content": "thinking_signature:abc123",
			"content":           []any{map[string]any{"type": "reasoning_text", "text": "the user said hello"}},
			"id":                "msg_x-0",
		},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hi"}}},
	}
	out := sanitizeChatGPTInput(input)
	if len(out) != 2 {
		t.Fatalf("cross-provider reasoning should be dropped, leaving 2 items, got %d", len(out))
	}
	for _, item := range out {
		if item.(map[string]any)["type"] == "reasoning" {
			t.Error("reasoning item with a foreign signature should be dropped")
		}
	}
}

// A reasoning item with no encrypted_content (e.g. only reasoning_text content)
// carries nothing the codex backend can use once content is emptied, so it too
// is dropped rather than sent as a bare shell.
func TestSanitizeChatGPTInput_ReasoningNoEncryptedDropped(t *testing.T) {
	input := []any{
		map[string]any{
			"type":    "reasoning",
			"summary": []any{},
			"content": []any{map[string]any{"type": "reasoning_text", "text": "thinking"}},
		},
	}
	if out := sanitizeChatGPTInput(input); len(out) != 0 {
		t.Fatalf("reasoning without encrypted_content should be dropped, got %d items", len(out))
	}
}

func TestSanitizeChatGPTInput_UnknownType(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "future_type", "data": "value",
			"id": "ft_1", "status": "completed",
		},
	}
	out := sanitizeChatGPTInput(input)
	m := out[0].(map[string]any)
	if _, ok := m["id"]; ok {
		t.Error("unknown type should have id stripped")
	}
	if _, ok := m["status"]; ok {
		t.Error("unknown type should have status stripped")
	}
	if m["data"] != "value" {
		t.Error("unknown type should preserve other fields")
	}
	if m["type"] != "future_type" {
		t.Error("unknown type should preserve type field")
	}
}

func TestSanitizeChatGPTInput_NonMapItem(t *testing.T) {
	input := []any{"just a string", 42}
	out := sanitizeChatGPTInput(input)
	if len(out) != 2 {
		t.Fatalf("expected 2 items, got %d", len(out))
	}
}

func TestSanitizeChatGPTInput_NestedContentUntouched(t *testing.T) {
	content := []any{map[string]any{
		"type": "output_text", "text": "hello",
		"annotations": []any{}, "logprobs": nil,
	}}
	input := []any{
		map[string]any{
			"type": "message", "role": "assistant", "content": content,
			"id": "msg_1", "status": "completed",
		},
	}
	out := sanitizeChatGPTInput(input)
	m := out[0].(map[string]any)
	c := m["content"].([]any)
	part := c[0].(map[string]any)
	if _, ok := part["annotations"]; !ok {
		t.Error("nested content part should keep annotations untouched")
	}
	if _, ok := part["logprobs"]; !ok {
		t.Error("nested content part should keep logprobs untouched")
	}
}
