package bridge

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
