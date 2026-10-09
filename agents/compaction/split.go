package compaction

import (
	"strings"

	"github.com/zzir/agents-go/agents/session"
)

// SafeSplit snaps a count-based split index to the nearest group boundary at or
// before it; 0 when no non-empty prefix is safe.
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
			// This group straddles the split; the last safe boundary is before it.
			return at
		}
		at = next
	}
	return at
}

// IsSummaryOnly reports whether entries amount to nothing but an existing
// compaction summary, which is not worth a model call to summarize again.
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
		if p.Role != "system" || !strings.Contains(session.RenderItem(e.Item), session.SummaryMarker) {
			return false
		}
	}
	return true
}
