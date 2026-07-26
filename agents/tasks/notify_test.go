package tasks

import (
	"strings"
	"testing"
)

// Formatting and parsing live together. This used to be a regex in the
// frontend, which meant the wire format was defined in one language and
// re-derived in another — and a wording change silently broke rendering.
func TestNotification_RoundTrips(t *testing.T) {
	in := []Task{
		{ID: "t1", Label: `the "big" job`, Status: StatusCompleted, Summary: "all good", Result: "all good"},
		{ID: "t2", Label: "other", Status: StatusFailed, Summary: "short", Result: strings.Repeat("x", 900)},
		{ID: "t3", Label: "quiet", Status: StatusCancelled},
	}
	msg := DefaultNotifyFormatter(in)

	n, ok := ParseNotification(msg)
	if !ok {
		t.Fatalf("did not parse:\n%s", msg)
	}
	if len(n.Tasks) != 3 {
		t.Fatalf("parsed %d tasks, want 3", len(n.Tasks))
	}
	if n.Tasks[0].TaskID != "t1" || n.Tasks[0].Status != StatusCompleted || n.Tasks[0].Summary != "all good" {
		t.Errorf("task 0 = %+v", n.Tasks[0])
	}
	// A result longer than its summary tells the model where the rest is.
	if !n.Tasks[1].Truncated {
		t.Error("the truncation marker was not reported")
	}
	if n.Tasks[1].Summary != "short" {
		t.Errorf("summary = %q, want the marker stripped", n.Tasks[1].Summary)
	}
	if n.Tasks[2].Summary != "" {
		t.Errorf("a task with no summary parsed one: %q", n.Tasks[2].Summary)
	}
}

func TestNotification_IgnoresOtherMessages(t *testing.T) {
	for _, s := range []string{
		"hello",
		"",
		NotificationPrefix, // prefix with nothing after it
		"Task \"x\" (id) completed.",
	} {
		if _, ok := ParseNotification(s); ok {
			t.Errorf("parsed a non-notification: %q", s)
		}
	}
}

// The notification is a user-role entry the model reads verbatim; the prefix is
// what lets a UI render it as a card rather than a user bubble.
func TestNotification_CarriesThePrefix(t *testing.T) {
	msg := DefaultNotifyFormatter([]Task{{ID: "t1", Label: "x", Status: StatusCompleted}})
	if !strings.HasPrefix(msg, NotificationPrefix) {
		t.Errorf("message = %q, want the prefix", msg)
	}
}
