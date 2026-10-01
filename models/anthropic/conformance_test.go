package anthropic

import (
	"encoding/json"
	"fmt"
	"maps"
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
				if !body.Stream {
					t.Error("the adapter sent a non-streaming request; Respond is served from the stream")
				}
				writeMessagesStream(t, w, s.Turn)
			}))
			t.Cleanup(srv.Close)
			provider := NewProvider(option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"))
			model, err := provider.Model("claude-test")
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
	if turn.Refusal != "" {
		// Reported out-of-band by stop_reason; the text block is the refusal.
		blocks = append(blocks, map[string]any{"type": "text", "text": turn.Refusal})
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
	case turn.Refusal != "":
		return "refusal"
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

func writeMessagesStream(t *testing.T, w http.ResponseWriter, turn conformancetest.TurnSpec) {
	t.Helper()
	// message_start carries input/cache counts but no output yet — the real
	// API reports output_tokens and the thinking breakdown in message_delta.
	// Keeping the fixture honest here matters: a fixture that front-loads the
	// final numbers would mask an adapter that reads them from the wrong event.
	startUsage := wireUsage(turn)
	startUsage["output_tokens"] = 0
	startUsage["output_tokens_details"] = map[string]any{"thinking_tokens": 0}
	stop := map[string]any{"stop_reason": wireStopReason(turn), "stop_sequence": nil}
	writeSSE(t, w, turn.ResponseID, wireBlocks(t, turn), stop, startUsage, wireUsage(turn))
}

// writeMessageSSE serves a fixture written as one whole Messages response as
// the event stream the API sends for it.
func writeMessageSSE(t *testing.T, w http.ResponseWriter, messageJSON string) {
	t.Helper()
	var msg struct {
		ID          string           `json:"id"`
		Content     []map[string]any `json:"content"`
		StopReason  string           `json:"stop_reason"`
		StopDetails map[string]any   `json:"stop_details"`
		Usage       map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal([]byte(messageJSON), &msg); err != nil {
		t.Fatal(err)
	}
	startUsage := maps.Clone(msg.Usage)
	startUsage["output_tokens"] = 0
	stop := map[string]any{"stop_reason": msg.StopReason, "stop_sequence": nil}
	if msg.StopDetails != nil {
		stop["stop_details"] = msg.StopDetails
	}
	writeSSE(t, w, msg.ID, msg.Content, stop, startUsage, msg.Usage)
}

// writeSSE streams one Messages response: message_start, each block as
// start/delta/stop events, then message_delta with stop and message_stop.
func writeSSE(t *testing.T, w http.ResponseWriter, id string, blocks []map[string]any, stop, startUsage, deltaUsage map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(eventType string, payload map[string]any) {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	}
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant",
			"model": "claude-test", "content": []any{}, "usage": startUsage,
		},
	})

	for i, block := range blocks {
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

	send("message_delta", map[string]any{"type": "message_delta", "delta": stop, "usage": deltaUsage})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func splitInTwo(text string) []string {
	if len(text) < 2 {
		return []string{text}
	}
	mid := len(text) / 2
	return []string{text[:mid], text[mid:]}
}
