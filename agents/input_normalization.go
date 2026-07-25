package agents

import "encoding/json"

// toolCallToOutputType maps tool-call input item types to the output item type
// that completes them, mirroring Python's _TOOL_CALL_TO_OUTPUT_TYPE. Go itself
// produces only function_call items, but stored history may have been written
// by the Python SDK (or by hand) using the hosted-tool item types, so the full
// table keeps cross-writer sessions replayable.
var toolCallToOutputType = map[string]string{
	"function_call":    "function_call_output",
	"custom_tool_call": "custom_tool_call_output",
	"shell_call":       "shell_call_output",
	"apply_patch_call": "apply_patch_call_output",
	"computer_call":    "computer_call_output",
	"local_shell_call": "local_shell_call_output",
	"tool_search_call": "tool_search_output",
}

// normalizeStoredInput scrubs items that entered the run from outside it —
// session history in prepareRun, a resumed state's original input — so replay
// cannot 400 at the Responses API: orphan tool calls (calls without a matching
// output, e.g. persisted by the Python SDK at an interruption) are dropped
// together with the reasoning items tied to them, then duplicate identified
// items collapse keeping the latest occurrence. Mirrors Python's
// drop_orphan_function_calls + deduplicate_input_items_preferring_latest.
func normalizeStoredInput(items []TResponseInputItem) []TResponseInputItem {
	if len(items) == 0 {
		return items
	}
	// One shared raw-JSON projection per item: the openai-go input union has no
	// uniform accessors across its variants, and the JSON form also covers
	// item types Go never constructs itself.
	itemMaps := make([]map[string]any, len(items))
	for i := range items {
		itemMaps[i] = inputItemAsMap(items[i])
	}
	items, itemMaps = dropOrphanToolCalls(items, itemMaps)
	return dedupeInputItemsPreferringLatest(items, itemMaps)
}

// inputItemAsMap projects an input item to a generic map via its JSON form;
// nil when the item does not marshal to a JSON object.
func inputItemAsMap(item TResponseInputItem) map[string]any {
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

// dropOrphanToolCalls removes tool-call items with no matching output item, so
// resumes and replays do not send dangling calls the API rejects ("No tool
// output found..."). A reasoning item tied to a dropped call — reasoning binds
// to the next non-reasoning item — is dropped with it, since the API likewise
// rejects reasoning "without its required following item". Calls without a
// call_id are kept (only hosted anonymous tool_search calls lack one; better
// to let the server decide than to over-prune).
func dropOrphanToolCalls(items []TResponseInputItem, itemMaps []map[string]any) ([]TResponseInputItem, []map[string]any) {
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
	// reasoning items collapse together (Python parity:
	// _drop_reasoning_items_preceding_dropped_calls).
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

	outItems := make([]TResponseInputItem, 0, len(items))
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

// inputItemDedupeKey derives a stable identity for deduplication, mirroring
// Python's _dedupe_key: messages (role-bearing or type=="message") are never
// deduped, placeholder fake ids are ignored so call_id-based dedupe still
// applies, and identity comes from id, then call_id, then approval_request_id.
// "" means "no identity — always keep".
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
func dedupeInputItemsPreferringLatest(items []TResponseInputItem, itemMaps []map[string]any) []TResponseInputItem {
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
	out := make([]TResponseInputItem, 0, kept)
	for i := range items {
		if keep[i] {
			out = append(out, items[i])
		}
	}
	return out
}
