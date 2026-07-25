package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
)

// fakeResponses is a minimal stand-in for the Responses API: enough for an
// example to complete a run, not enough to be a mock of the real thing.
//
// The point is to execute the examples, not to assert on model behavior. So it
// answers with the shortest plausible response for whatever the example asked:
// a tool call when tools are offered and none has been called yet, plain text
// otherwise. Each conversation is keyed by its own turn count, so an example
// that loops (tool → result → answer) terminates instead of calling the same
// tool forever.
type fakeResponses struct {
	mu    sync.Mutex
	turns int
}

type responsesRequest struct {
	Stream bool             `json:"stream"`
	Model  string           `json:"model"`
	Tools  []map[string]any `json:"tools"`
	Input  json.RawMessage  `json:"input"`
}

func (f *fakeResponses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Anything that is not a response creation (conversations, prompts, files)
	// is out of scope; say so loudly rather than returning a shape the SDK will
	// misparse into a confusing error.
	if !strings.HasSuffix(r.URL.Path, "/responses") {
		http.Error(w, fmt.Sprintf("verifyexamples fake: unsupported path %q", r.URL.Path), http.StatusNotFound)
		return
	}

	var req responsesRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	f.turns++
	turn := f.turns
	f.mu.Unlock()

	// Offer a tool call on the first turn only. The second turn answers, which
	// is what ends the run loop.
	callTool := len(req.Tools) > 0 && turn == 1
	resp := f.response(req, callTool, turn)

	if req.Stream {
		f.stream(w, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

const fakeText = "verifyexamples: ok"

func (f *fakeResponses) response(req responsesRequest, callTool bool, turn int) map[string]any {
	var output []any
	if callTool {
		name, _ := req.Tools[0]["name"].(string)
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%d", turn),
			"call_id":   fmt.Sprintf("call_%d", turn),
			"name":      name,
			"arguments": defaultArgs(req.Tools[0]),
			"status":    "completed",
		})
	} else {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     fmt.Sprintf("msg_%d", turn),
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": fakeText, "annotations": []any{},
			}},
		})
	}
	model := req.Model
	if model == "" {
		model = "gpt-4o"
	}
	return map[string]any{
		"id":         fmt.Sprintf("resp_%d", turn),
		"object":     "response",
		"created_at": 0,
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
		},
	}
}

// defaultArgs builds an arguments object satisfying the tool's required fields
// with zero values. A strict-mode schema rejects a bare "{}" when it has
// required properties, and the SDK would surface that as a validation error
// rather than running the example's tool.
func defaultArgs(tool map[string]any) string {
	schema, _ := tool["parameters"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)

	args := map[string]any{}
	for _, r := range required {
		name, _ := r.(string)
		spec, _ := props[name].(map[string]any)
		args[name] = zeroForSchema(spec)
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func zeroForSchema(spec map[string]any) any {
	switch t, _ := spec["type"].(string); t {
	case "number", "integer":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default:
		return "verifyexamples"
	}
}

// stream writes the response as SSE. Only the events the SDK needs to build a
// result are emitted — a real stream carries many more.
func (f *fakeResponses) stream(w http.ResponseWriter, resp map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	send := func(event string, payload any) {
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	created := maps.Clone(resp)
	created["status"] = "in_progress"
	created["output"] = []any{}
	send("response.created", map[string]any{
		"type": "response.created", "sequence_number": 0, "response": created,
	})

	// Only text output gets deltas; a function call arrives whole in the
	// completed event, which is enough for the SDK to dispatch it.
	if out, _ := resp["output"].([]any); len(out) == 1 {
		if item, _ := out[0].(map[string]any); item["type"] == "message" {
			send("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "sequence_number": 1,
				"item_id": item["id"], "output_index": 0, "content_index": 0,
				"delta": fakeText,
			})
		}
	}

	send("response.completed", map[string]any{
		"type": "response.completed", "sequence_number": 2, "response": resp,
	})
}
