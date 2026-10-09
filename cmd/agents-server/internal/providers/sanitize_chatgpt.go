package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3/option"
)

// The ChatGPT codex backend's request shape: the middleware that rewrites a
// Responses request for it, and the input sanitizer the rewrite applies.

// sanitizeChatGPTInput keeps, per known item type, only the fields the codex
// backend accepts (unknown types lose id and status); item_reference is
// dropped. Message content passes through untouched — see
// docs/reference/protocol.md, ChatGPT OAuth.
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
		if s, keep := sanitizeChatGPTItem(m); keep {
			out = append(out, s)
		}
	}
	return out
}

// sanitizeChatGPTItem returns the cleaned item and whether to keep it; a
// reasoning item the codex backend can't use is dropped (keep=false).
func sanitizeChatGPTItem(m map[string]any) (map[string]any, bool) {
	switch m["type"] {
	case "message":
		return pick(m, "type", "role", "content"), true
	case "function_call":
		return pick(m, "type", "call_id", "name", "arguments"), true
	case "function_call_output":
		return pick(m, "type", "call_id", "output"), true
	case "reasoning":
		// Only a codex-native encrypted_content survives, its content emptied;
		// another provider's reasoning (no blob, or a thinking_signature: one)
		// is dropped.
		enc, _ := m["encrypted_content"].(string)
		if enc == "" || strings.HasPrefix(enc, "thinking_signature:") {
			return nil, false
		}
		r := pick(m, "type", "summary", "encrypted_content")
		r["content"] = []any{}
		return r, true
	default:
		return stripResponseMeta(m), true
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

// stripResponseMeta is the fallback for unknown item types: drop the two
// response-only metadata fields, keep the rest.
func stripResponseMeta(m map[string]any) map[string]any {
	dst := make(map[string]any, len(m))
	maps.Copy(dst, m)
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
