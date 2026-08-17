package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3/option"
)

// The ChatGPT codex backend's request shape: the middleware that rewrites a
// Responses request for it, and the input sanitizer the rewrite applies.

// sanitizeChatGPTInput cleans a Responses-API input array for the ChatGPT
// codex backend, which rejects fields the standard OpenAI API silently
// ignores.  The approach is per-type allowlist: for each known item type only
// the fields the backend accepts are kept; unknown types get a conservative
// fallback (strip id + status, pass the rest through).
//
// Nested content/summary arrays are NOT cleaned — the codex backend accepts
// its own nested format and external providers (e.g. Volcengine) produce
// minimal nested parts anyway.  If a future 400 points at a nested path
// (input[N].content[M].xxx), extend the per-type branch to walk the array.
func sanitizeChatGPTInput(input []any) []any {
	out := make([]any, 0, len(input))
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		if m["type"] == "item_reference" {
			continue
		}
		out = append(out, sanitizeChatGPTItem(m))
	}
	return out
}

func sanitizeChatGPTItem(m map[string]any) map[string]any {
	switch m["type"] {
	case "message":
		return pick(m, "type", "role", "content")
	case "function_call":
		return pick(m, "type", "call_id", "name", "arguments")
	case "function_call_output":
		return pick(m, "type", "call_id", "output")
	case "reasoning":
		return pick(m, "type", "content", "summary", "encrypted_content")
	default:
		return stripResponseMeta(m)
	}
}

// pick returns a new map containing only the listed keys that exist in src.
func pick(src map[string]any, keys ...string) map[string]any {
	dst := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
	return dst
}

// stripResponseMeta is the conservative fallback for unknown item types:
// remove the two universal response-only metadata fields and keep everything
// else, so new item types aren't silently dropped.
func stripResponseMeta(m map[string]any) map[string]any {
	dst := make(map[string]any, len(m))
	for k, v := range m {
		dst[k] = v
	}
	delete(dst, "id")
	delete(dst, "status")
	return dst
}

func newChatGPTMiddleware(accountID string) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if accountID != "" {
			req.Header.Set("ChatGPT-Account-ID", accountID)
		}
		req.Header.Set("originator", "codex_cli_rs")

		if req.Body != nil && req.Method == http.MethodPost {
			raw, err := io.ReadAll(req.Body)
			req.Body.Close()
			if err != nil {
				// The body is consumed and closed: forwarding would fail
				// downstream with an unrelated error, hiding this one.
				return nil, fmt.Errorf("chatgpt: reading the request body: %w", err)
			}
			var body map[string]any
			if json.Unmarshal(raw, &body) == nil {
				body["store"] = false
				delete(body, "previous_response_id")
				if input, ok := body["input"].([]any); ok {
					body["input"] = sanitizeChatGPTInput(input)
				}
				patched, _ := json.Marshal(body)
				raw = patched
			}
			req.Body = io.NopCloser(strings.NewReader(string(raw)))
			req.ContentLength = int64(len(raw))
		}

		return next(req)
	}
}
