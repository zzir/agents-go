package agents

import "testing"

// The classifiers decide which events may be dropped when an attempt is
// replaced, so what matters is membership, not just spelling.
//
// response.queued is the reason this test exists: it is the one name the three
// hand-written vocabularies had already drifted on, and unlike created /
// in_progress / failed no decorator test feeds it as a literal — its
// "tolerated wherever created and in_progress are" status lived only in a
// comment. Event names are spelled out here rather than taken from the
// constants, so this stays an independent statement of the rule.
func TestStreamLifecycleEvent(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"response.created", true},
		{"response.in_progress", true},
		{"response.queued", true},
		{"response.output_text.delta", false},
		{"response.output_item.added", false},
		{"response.completed", false},
		{"response.failed", false},
	} {
		if got := streamLifecycleEvent(tc.typ); got != tc.want {
			t.Errorf("streamLifecycleEvent(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

// response.incomplete is the load-bearing non-member: a length-truncated
// response is output that ARRIVED, so committing the attempt on it is what
// stops a retry from throwing that output away.
func TestStreamFailureEvent(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"error", true},
		{"response.error", true},
		{"response.failed", true},
		{"response.incomplete", false},
		{"response.completed", false},
		{"response.queued", false},
	} {
		if got := streamFailureEvent(tc.typ); got != tc.want {
			t.Errorf("streamFailureEvent(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}
