package session

import "encoding/json"

// pendingCallIDs returns, in call order, the tool calls entries record without
// an output (a paused or a crashed run).
func pendingCallIDs(entries []Entry) []string {
	open := map[string]bool{}
	var order []string
	for _, e := range entries {
		if e.Kind != EntryKindItem {
			continue
		}
		callID, isCall, isOutput := entryCallID(e)
		switch {
		case isCall && callID != "":
			if !open[callID] {
				open[callID] = true
				order = append(order, callID)
			}
		case isOutput && callID != "":
			open[callID] = false
		}
	}
	var pending []string
	for _, id := range order {
		if open[id] {
			pending = append(pending, id)
		}
	}
	return pending
}

// AddRequestUsage accumulates src into dst, field by field: the one definition
// of summing usage.
func AddRequestUsage(dst *RequestUsage, src *RequestUsage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalTokens += src.TotalTokens
	dst.InputTokensDetails.CachedTokens += src.InputTokensDetails.CachedTokens
	dst.InputTokensDetails.CacheWriteTokens += src.InputTokensDetails.CacheWriteTokens
	dst.OutputTokensDetails.ReasoningTokens += src.OutputTokensDetails.ReasoningTokens
}

// ItemProbe is the classifying fields of a stored item, read off its wire JSON
// without decoding the union; a field the item lacks is zero.
type ItemProbe struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	CallID  string          `json:"call_id"`
	Name    string          `json:"name"`
	Args    string          `json:"arguments"`
	Content json.RawMessage `json:"content"`
	Output  json.RawMessage `json:"output"`
	Summary json.RawMessage `json:"summary"`
}

// ProbeItem reads the classifying fields of an item's wire JSON; malformed or
// empty JSON probes as the zero value.
func ProbeItem(item json.RawMessage) ItemProbe {
	var p ItemProbe
	if len(item) > 0 {
		_ = json.Unmarshal(item, &p)
	}
	return p
}

// entryCallID reports an item entry's tool call id and whether it is a call or
// an output.
func entryCallID(e Entry) (id string, isCall, isOutput bool) {
	switch p := ProbeItem(e.Item); p.Type {
	case "function_call":
		return p.CallID, true, false
	case "function_call_output":
		return p.CallID, false, true
	}
	return "", false, false
}
