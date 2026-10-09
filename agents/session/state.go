package session

import "encoding/json"

// DerivedState is what a session's entries add up to, folded from them and
// never stored — spec §2.5c.
type DerivedState struct {
	// LastAgent is the agent that produced the most recent entry.
	LastAgent string
	// LastResponseID is the most recent model call's identifier.
	LastResponseID string
	// PendingCallIDs are tool calls recorded without their outputs (a paused or
	// a crashed run).
	PendingCallIDs []string
	// Usage totals the token usage the entries carry; Requests counts the
	// entries that carried it.
	Usage    RequestUsage
	Requests int
}

// Stats summarizes a session cheaply enough to show in a list.
type Stats struct {
	// Entries is the total number of entries.
	Entries int
	// Items is how many of those are conversation items.
	Items int
	// Annotations is how many are display-only.
	Annotations int
	// Compactions is how many compaction checkpoints the session holds.
	Compactions int
	// Usage totals the token usage across the session.
	Usage    RequestUsage
	Requests int
}

// ReduceState folds entries into the state they imply; a pure function of the entries.
func ReduceState(entries []Entry) DerivedState {
	var st DerivedState
	open := map[string]bool{}
	var order []string

	for _, e := range entries {
		if e.AgentName != "" {
			st.LastAgent = e.AgentName
		}
		if e.ResponseID != "" {
			st.LastResponseID = e.ResponseID
		}
		if e.Usage != nil {
			AddRequestUsage(&st.Usage, e.Usage)
			st.Requests++
		}
		if e.Kind != EntryKindItem {
			continue
		}
		// A call is pending until its output lands.
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

	for _, id := range order {
		if open[id] {
			st.PendingCallIDs = append(st.PendingCallIDs, id)
		}
	}
	return st
}

// StatsOf summarizes entries without decoding their payloads.
func StatsOf(entries []Entry) Stats {
	st := Stats{Entries: len(entries)}
	for _, e := range entries {
		switch e.Kind {
		case EntryKindItem:
			st.Items++
		case EntryKindAnnotation:
			st.Annotations++
		case EntryKindCompaction:
			st.Compactions++
		}
		if e.Usage != nil {
			AddRequestUsage(&st.Usage, e.Usage)
			st.Requests++
		}
	}
	return st
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
