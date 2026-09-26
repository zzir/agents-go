package agents

import (
	"encoding/json"
	"slices"

	"github.com/zzir/agents-go/agents/session"
)

// toolCallToOutputType maps tool-call input item types to the output item type
// that completes them. This SDK produces only function_call items, but stored
// history may have been written by another Responses client (or by hand) using
// the hosted-tool item types, so the full table keeps such sessions replayable.
var toolCallToOutputType = map[string]string{
	"function_call":    "function_call_output",
	"custom_tool_call": "custom_tool_call_output",
	"shell_call":       "shell_call_output",
	"apply_patch_call": "apply_patch_call_output",
	"computer_call":    "computer_call_output",
	"local_shell_call": "local_shell_call_output",
	"tool_search_call": "tool_search_output",
}

// normalizeStoredInput scrubs items from outside the run (session history, a resumed
// state's input): orphan tool calls and their reasoning go, duplicates keep the latest.
func normalizeStoredInput(items []InputItem) []InputItem {
	if len(items) == 0 {
		return items
	}
	// One shared raw-JSON projection per item: the openai-go input union has no
	// uniform accessors across its variants.
	itemMaps := make([]map[string]any, len(items))
	for i := range items {
		itemMaps[i] = inputItemAsMap(items[i])
	}
	items, itemMaps = dropOrphanToolCalls(items, itemMaps)
	items = stripStoredLogprobs(items, itemMaps)
	return dedupeInputItemsPreferringLatest(items, itemMaps)
}

// stripMessageLogprobs removes logprobs from an assistant message's content
// parts; false when raw is not a message carrying any — see spec §2.1b.
func stripMessageLogprobs(raw []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	var typ string
	if json.Unmarshal(raw, &m) != nil || json.Unmarshal(m["type"], &typ) != nil || typ != "message" {
		return raw, false
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(m["content"], &parts) != nil {
		return raw, false
	}
	changed := false
	for _, p := range parts {
		if _, ok := p["logprobs"]; ok {
			delete(p, "logprobs")
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	content, err := json.Marshal(parts)
	if err != nil {
		return raw, false
	}
	m["content"] = content
	out, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return out, true
}

// stripStoredLogprobs rebuilds each stored assistant message whose content
// parts carry logprobs; the caller's slice is left untouched.
func stripStoredLogprobs(items []InputItem, itemMaps []map[string]any) []InputItem {
	var out []InputItem
	for i, m := range itemMaps {
		if !messageHasLogprobs(m) {
			continue
		}
		raw, err := json.Marshal(items[i])
		if err != nil {
			continue
		}
		cleaned, changed := stripMessageLogprobs(raw)
		if !changed {
			continue
		}
		item, err := session.UnmarshalInputItem(cleaned)
		if err != nil {
			continue
		}
		if out == nil {
			out = slices.Clone(items)
		}
		out[i] = item
	}
	if out == nil {
		return items
	}
	return out
}

func messageHasLogprobs(m map[string]any) bool {
	if m == nil || m["type"] != "message" {
		return false
	}
	parts, _ := m["content"].([]any)
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			if _, has := pm["logprobs"]; has {
				return true
			}
		}
	}
	return false
}

// inputItemAsMap projects an input item to a generic map via its JSON form;
// nil when the item does not marshal to a JSON object.
func inputItemAsMap(item InputItem) map[string]any {
	b, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// dropOrphanToolCalls removes tool calls with no matching output — the API rejects
// them — plus the reasoning bound to each; a call without a call_id is kept.
func dropOrphanToolCalls(items []InputItem, itemMaps []map[string]any) ([]InputItem, []map[string]any) {
	completed := map[string]bool{} // outputType + call id
	for _, m := range itemMaps {
		if m == nil {
			continue
		}
		typ, _ := m["type"].(string)
		for _, outType := range toolCallToOutputType {
			if typ != outType {
				continue
			}
			if cid, ok := m["call_id"].(string); ok && cid != "" {
				completed[outType+":"+cid] = true
			}
			break
		}
	}

	dropped := make([]bool, len(items))
	droppedCount := 0
	for i, m := range itemMaps {
		if m == nil {
			continue
		}
		typ, _ := m["type"].(string)
		outType, isCall := toolCallToOutputType[typ]
		if !isCall {
			continue
		}
		cid, _ := m["call_id"].(string)
		if cid == "" || completed[outType+":"+cid] {
			continue
		}
		dropped[i] = true
		droppedCount++
	}
	if droppedCount == 0 {
		return items, itemMaps
	}

	// A reasoning item is tied to the next non-reasoning item; if that item was
	// just dropped, the reasoning item dangles too. Scan backward so chained
	// reasoning items collapse together.
	dropReasoning := make([]bool, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		m := itemMaps[i]
		if m == nil || m["type"] != "reasoning" || dropped[i] {
			continue
		}
		for next := i + 1; next < len(items); next++ {
			if dropReasoning[next] {
				continue
			}
			nm := itemMaps[next]
			if nm != nil && nm["type"] == "reasoning" {
				continue
			}
			if dropped[next] {
				dropReasoning[i] = true
			}
			break
		}
	}

	outItems := make([]InputItem, 0, len(items))
	outMaps := make([]map[string]any, 0, len(itemMaps))
	for i := range items {
		if dropped[i] || dropReasoning[i] {
			continue
		}
		outItems = append(outItems, items[i])
		outMaps = append(outMaps, itemMaps[i])
	}
	return outItems, outMaps
}

// inputItemDedupeKey derives a dedupe identity from id, then call_id, then
// approval_request_id; "" (a message, or no identity) means always keep.
func inputItemDedupeKey(m map[string]any) string {
	if m == nil {
		return ""
	}
	if _, hasRole := m["role"]; hasRole {
		return ""
	}
	typ, _ := m["type"].(string)
	if typ == "message" {
		return ""
	}
	if id, ok := m["id"].(string); ok && id != "" {
		return "id:" + typ + ":" + id
	}
	if cid, ok := m["call_id"].(string); ok && cid != "" {
		return "call_id:" + typ + ":" + cid
	}
	if arid, ok := m["approval_request_id"].(string); ok && arid != "" {
		return "approval_request_id:" + typ + ":" + arid
	}
	return ""
}

// dedupeInputItemsPreferringLatest collapses items sharing a stable identity,
// keeping the LATEST occurrence (a re-sent item supersedes the stored copy).
func dedupeInputItemsPreferringLatest(items []InputItem, itemMaps []map[string]any) []InputItem {
	seen := map[string]bool{}
	keep := make([]bool, len(items))
	kept := 0
	for i := len(items) - 1; i >= 0; i-- {
		key := inputItemDedupeKey(itemMaps[i])
		if key != "" && seen[key] {
			continue
		}
		if key != "" {
			seen[key] = true
		}
		keep[i] = true
		kept++
	}
	if kept == len(items) {
		return items
	}
	out := make([]InputItem, 0, kept)
	for i := range items {
		if keep[i] {
			out = append(out, items[i])
		}
	}
	return out
}
