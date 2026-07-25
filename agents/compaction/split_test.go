package compaction

import (
	"testing"

	"github.com/zzir/agents-go/agents"
)

// The whole reason SafeSplit exists: a count-based cut lands between a tool
// call and its output, which would make the summary request invalid and leave
// the kept history starting on an orphaned output.
func TestSafeSplit_NeverCutsAToolCallFromItsOutput(t *testing.T) {
	entries := withIDs([]agents.SessionEntry{
		user(t, "do it"),
		call(t, "c1", "run"),
		output(t, "c1", "done"),
		user(t, "thanks"),
	})

	// Asking to cut between the call and its output moves back to the boundary
	// before the pair.
	if got := SafeSplit(entries, 2); got != 1 {
		t.Errorf("SafeSplit(…, 2) = %d, want 1 (before the call/output pair)", got)
	}
	// A split that already sits on a boundary is left alone.
	if got := SafeSplit(entries, 3); got != 3 {
		t.Errorf("SafeSplit(…, 3) = %d, want 3", got)
	}
	if got := SafeSplit(entries, 1); got != 1 {
		t.Errorf("SafeSplit(…, 1) = %d, want 1", got)
	}
}

// Reasoning that led to a tool call belongs with it, and a split that would
// separate them moves back too — a pairing the old hand-written walker needed a
// second rule for.
func TestSafeSplit_KeepsReasoningWithItsCall(t *testing.T) {
	entries := withIDs([]agents.SessionEntry{
		user(t, "do it"),
		reasoning(t),
		call(t, "c1", "run"),
		output(t, "c1", "done"),
	})
	if got := SafeSplit(entries, 3); got != 1 {
		t.Errorf("SafeSplit(…, 3) = %d, want 1 (reasoning + call + output are one group)", got)
	}
}

func TestSafeSplit_Bounds(t *testing.T) {
	entries := withIDs([]agents.SessionEntry{user(t, "a"), user(t, "b")})
	if got := SafeSplit(entries, 0); got != 0 {
		t.Errorf("SafeSplit(…, 0) = %d, want 0", got)
	}
	if got := SafeSplit(nil, 3); got != 0 {
		t.Errorf("SafeSplit(nil, 3) = %d, want 0", got)
	}
	if got := SafeSplit(entries, 99); got != len(entries) {
		t.Errorf("SafeSplit(…, 99) = %d, want %d", got, len(entries))
	}
}

// Summarizing a summary produces a paraphrase of a paraphrase; a conversation
// that does it every pass decays.
func TestIsSummaryOnly(t *testing.T) {
	summary := item(t, `{"role":"system","content":`+quote(agents.SummaryMarker+` earlier`)+`}`)

	if !IsSummaryOnly([]agents.SessionEntry{summary}) {
		t.Error("a lone summary was not recognized")
	}
	if IsSummaryOnly([]agents.SessionEntry{summary, user(t, "and then?")}) {
		t.Error("a summary followed by real conversation counted as summary-only")
	}
	if IsSummaryOnly(nil) {
		t.Error("an empty prefix counted as summary-only")
	}
	if IsSummaryOnly([]agents.SessionEntry{user(t, "hello")}) {
		t.Error("a plain user message counted as summary-only")
	}
}
