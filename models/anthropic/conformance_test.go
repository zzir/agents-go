package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit/conformancetest"
)

// TestConformance runs the shared adapter matrix against MessagesModel,
// backed by a fake Messages API server whose fixtures are hand-written
// Anthropic wire JSON/SSE — the translation under test is exactly the gap
// between those fixtures and the canonical assertions.
func TestConformance(t *testing.T) {
	conformancetest.Run(t, conformancetest.Target{
		NewModel: func(t *testing.T, s conformancetest.Scenario) agents.Model {
			t.Helper()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "messages") {
					http.NotFound(w, r)
					return
				}
				var body struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if body.Stream {
					writeMessagesStream(t, w, s.Turn)
				} else {
					writeMessagesResponse(t, w, s.Turn)
				}
			}))
			t.Cleanup(srv.Close)
			provider := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
			model, err := provider.GetModel("claude-test")
			if err != nil {
				t.Fatal(err)
			}
			return model
		},
	})
}

// wireBlocks builds the Anthropic content blocks meaning what the turn spec
// says, in canonical order (thinking, text, tool calls).
func wireBlocks(t *testing.T, turn conformancetest.TurnSpec) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	if turn.Reasoning != nil {
		blocks = append(blocks, map[string]any{
			"type":      "thinking",
			"thinking":  turn.Reasoning.Text,
			"signature": turn.Reasoning.Encrypted,
		})
	}
	if turn.Text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": turn.Text})
	}
	for _, call := range turn.ToolCalls {
		var input any
		if err := json.Unmarshal([]byte(call.ArgumentsJSON), &input); err != nil {
			t.Fatalf("scenario arguments %q: %v", call.ArgumentsJSON, err)
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": call.CallID, "name": call.Name, "input": input,
		})
	}
	return blocks
}

func wireStopReason(turn conformancetest.TurnSpec) string {
	switch {
	case turn.Truncated:
		return "max_tokens"
	case len(turn.ToolCalls) > 0:
		return "tool_use"
	default:
		return "end_turn"
	}
}

// wireUsage converts canonical usage numbers back into Anthropic's split
// accounting: wire input_tokens excludes cache reads and writes.
func wireUsage(turn conformancetest.TurnSpec) map[string]any {
	return map[string]any{
		"input_tokens":                turn.Usage.Input - turn.Usage.CachedRead - turn.Usage.CacheWrite,
		"output_tokens":               turn.Usage.Output,
		"cache_read_input_tokens":     turn.Usage.CachedRead,
		"cache_creation_input_tokens": turn.Usage.CacheWrite,
		"output_tokens_details":       map[string]any{"thinking_tokens": turn.Usage.Reasoning},
	}
}

func writeMessagesResponse(t *testing.T, w http.ResponseWriter, turn conformancetest.TurnSpec) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]any{
		"id":          turn.ResponseID,
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-test",
		"content":     wireBlocks(t, turn),
		"stop_reason": wireStopReason(turn),
		"usage":       wireUsage(turn),
	})
	if err != nil {
		t.Error(err)
	}
}

func writeMessagesStream(t *testing.T, w http.ResponseWriter, turn conformancetest.TurnSpec) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(eventType string, payload map[string]any) {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	}

	// message_start carries input/cache counts but no output yet — the real
	// API reports output_tokens and the thinking breakdown in message_delta.
	// Keeping the fixture honest here matters: a fixture that front-loads the
	// final numbers would mask an adapter that reads them from the wrong event.
	startUsage := wireUsage(turn)
	startUsage["output_tokens"] = 0
	startUsage["output_tokens_details"] = map[string]any{"thinking_tokens": 0}
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": turn.ResponseID, "type": "message", "role": "assistant",
			"model": "claude-test", "content": []any{}, "usage": startUsage,
		},
	})

	for i, block := range wireBlocks(t, turn) {
		switch block["type"] {
		case "text":
			send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			for _, chunk := range splitInTwo(block["text"].(string)) {
				send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "text_delta", "text": chunk},
				})
			}
		case "thinking":
			send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			})
			for _, chunk := range splitInTwo(block["thinking"].(string)) {
				send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "thinking_delta", "thinking": chunk},
				})
			}
			send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "signature_delta", "signature": block["signature"]},
			})
		case "tool_use":
			send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{
					"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{},
				},
			})
			args, err := json.Marshal(block["input"])
			if err != nil {
				t.Fatal(err)
			}
			for _, chunk := range splitInTwo(string(args)) {
				send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": chunk},
				})
			}
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}

	deltaUsage := wireUsage(turn)
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": wireStopReason(turn), "stop_sequence": nil},
		"usage": deltaUsage,
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func splitInTwo(text string) []string {
	if len(text) < 2 {
		return []string{text}
	}
	mid := len(text) / 2
	return []string{text[:mid], text[mid:]}
}
