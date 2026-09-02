package compaction

import (
	"encoding/json"
	"strings"

	"github.com/zzir/agents-go/agents/session"
)

// SafeSplit snaps a count-based split index to the nearest group boundary at or
// before it, so neither side holds half of something that must stay together.
//
// It returns 0 when no non-empty prefix is safe — the caller should skip rather
// than split somewhere that corrupts the history.
func SafeSplit(entries []session.Entry, split int) int {
	if split <= 0 || len(entries) == 0 {
		return 0
	}
	if split >= len(entries) {
		return len(entries)
	}

	idx := NewIndex(entries, nil)
	at := 0
	for _, g := range idx.Groups {
		next := at + len(g.Entries)
		if next > split {
			// This group straddles the requested split, so the last safe
			// boundary is the one before it.
			return at
		}
		at = next
	}
	return at
}

// IsSummaryOnly reports whether entries amount to nothing but an existing
// compaction summary.
//
// Summarizing that produces a summary of a summary, and a conversation that
// does it every pass decays into a paraphrase of a paraphrase. Callers check
// this before spending a model call.
func IsSummaryOnly(entries []session.Entry) bool {
	if len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if e.Kind == session.EntryKindCompaction {
			continue
		}
		if kind, _, _, _ := classify(e); kind == GroupOther {
			continue
		}
		p := session.ProbeItem(e.Item)
		if p.Role != "system" || !strings.Contains(entryText(e), session.SummaryMarker) {
			return false
		}
	}
	return true
}

// entryText pulls an entry's readable text, whether its content is a bare
// string or the parts array the Responses API also accepts.
func entryText(e session.Entry) string {
	p := session.ProbeItem(e.Item)
	if len(p.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(p.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(p.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.Text)
	}
	return b.String()
}
