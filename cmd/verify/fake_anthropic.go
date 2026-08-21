package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// fakeMessages is the Anthropic half of the fake backend: a minimal stand-in
// for the Messages API, with the same philosophy as fakeResponses — the
// shortest plausible answer that lets an example complete, not a mock.
type fakeMessages struct {
	mu    sync.Mutex
	turns int
}

type messagesRequest struct {
	Stream bool             `json:"stream"`
	Model  string           `json:"model"`
	Tools  []map[string]any `json:"tools"`
}

func (f *fakeMessages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/messages") {
		http.Error(w, fmt.Sprintf("verify fake: unsupported path %q", r.URL.Path), http.StatusNotFound)
		return
	}

	var req messagesRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	f.turns++
	turn := f.turns
	f.mu.Unlock()

	callTool := len(req.Tools) > 0 && turn == 1
	resp := f.message(req, callTool, turn)

	if req.Stream {
		f.stream(w, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeMessages) message(req messagesRequest, callTool bool, turn int) map[string]any {
	var content []any
	stopReason := "end_turn"
	if callTool {
		name, _ := req.Tools[0]["name"].(string)
		var input map[string]any
		// Anthropic tools carry the schema as input_schema (the Responses
		// fake's defaultArgs reads "parameters", so rebuild here).
		tool := map[string]any{"name": name, "parameters": req.Tools[0]["input_schema"]}
		_ = json.Unmarshal([]byte(defaultArgs(tool)), &input)
		content = append(content, map[string]any{
			"type": "tool_use", "id": fmt.Sprintf("toolu_%d", turn), "name": name, "input": input,
		})
		stopReason = "tool_use"
	} else {
		content = append(content, map[string]any{"type": "text", "text": fakeText})
	}
	return map[string]any{
		"id":          fmt.Sprintf("msg_%d", turn),
		"type":        "message",
		"role":        "assistant",
		"model":       req.Model,
		"content":     content,
		"stop_reason": stopReason,
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	}
}

// stream writes the message as the Messages SSE event sequence.
func (f *fakeMessages) stream(w http.ResponseWriter, resp map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	send := func(event string, payload map[string]any) {
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	start := map[string]any{
		"id": resp["id"], "type": "message", "role": "assistant", "model": resp["model"],
		"content": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
	}
	send("message_start", map[string]any{"type": "message_start", "message": start})

	content, _ := resp["content"].([]any)
	for i, c := range content {
		block, _ := c.(map[string]any)
		switch block["type"] {
		case "text":
			send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "text_delta", "text": block["text"]},
			})
		case "tool_use":
			send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{
					"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{},
				},
			})
			args, _ := json.Marshal(block["input"])
			send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)},
			})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}

	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": resp["stop_reason"], "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 1},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}
