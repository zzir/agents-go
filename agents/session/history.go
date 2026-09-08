package session

import (
	"context"
	"strings"
)

// HistoryQuery selects item entries of a session's active branch, folded ones
// included, newest first (spec §2.5i).
type HistoryQuery struct {
	// Query is a case-insensitive literal substring of RenderItem's text; ""
	// matches every item.
	Query string
	// Role keeps one ItemRole ("user", "assistant", "tool"); "" keeps all.
	Role string
	// ToolName keeps the calls of one tool and their outputs; "" keeps all.
	ToolName string
	// Limit caps the hits; <= 0 means DefaultHistoryLimit.
	Limit int
	// Before keeps only entries older than this entry id; "" starts at the newest.
	Before string
}

// DefaultHistoryLimit is the hits a query returns when it names no limit.
const DefaultHistoryLimit = 20

// HistorySearcher is an optional Storage capability: answer a HistoryQuery
// without loading every entry body. A storage that excludes folded entries
// from Entries must implement it, or the folded history is unsearchable.
type HistorySearcher interface {
	SearchHistory(ctx context.Context, q HistoryQuery) (hits []Entry, more bool, err error)
}

// SearchHistory answers q: the storage's own HistorySearcher when it has one,
// else every entry read and filtered here.
func (s *Session) SearchHistory(ctx context.Context, q HistoryQuery) ([]Entry, bool, error) {
	if hs, ok := s.storage.(HistorySearcher); ok {
		return hs.SearchHistory(ctx, q)
	}
	all, err := s.storage.Entries(ctx, Cursor{})
	if err != nil {
		return nil, false, err
	}
	hits, more := SearchHistory(all, q)
	return hits, more, nil
}

// SearchHistory answers q from entries in append order: the active branch's
// item entries, newest first. more reports that hits were cut at the limit.
func SearchHistory(entries []Entry, q HistoryQuery) (hits []Entry, more bool) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	path := ActiveBranchOf(entries)
	if q.Before != "" {
		end := -1
		for i := range path {
			if path[i].ID == q.Before {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, false
		}
		path = path[:end]
	}
	calls := CallToolNames(path)
	for i := len(path) - 1; i >= 0; i-- {
		if !MatchesHistory(path[i], q, calls) {
			continue
		}
		if len(hits) == limit {
			return hits, true
		}
		hits = append(hits, path[i])
	}
	return hits, false
}

// CallToolNames maps each function call's call id to its tool name, which is
// how an output learns which tool produced it.
func CallToolNames(entries []Entry) map[string]string {
	var names map[string]string
	for _, e := range entries {
		if e.Kind != EntryKindItem {
			continue
		}
		p := ProbeItem(e.Item)
		if p.Type != "function_call" || p.CallID == "" {
			continue
		}
		if names == nil {
			names = make(map[string]string)
		}
		names[p.CallID] = p.Name
	}
	return names
}

// MatchesHistory reports whether e satisfies q's filters, the one predicate
// every search path applies; calls is CallToolNames of the entries in view.
func MatchesHistory(e Entry, q HistoryQuery, calls map[string]string) bool {
	if e.Kind != EntryKindItem {
		return false
	}
	text := RenderItem(e.Item)
	if text == "" {
		return false
	}
	role := ItemRole(e.Item)
	if q.Role != "" && role != q.Role {
		return false
	}
	if q.ToolName != "" {
		p := ProbeItem(e.Item)
		name := p.Name
		if p.Type == "function_call_output" {
			name = calls[p.CallID]
		}
		if role != "tool" || name != q.ToolName {
			return false
		}
	}
	return q.Query == "" || strings.Contains(strings.ToLower(text), strings.ToLower(q.Query))
}
