package tasks

import (
	"strings"
	"testing"
)

// One task, one line; the wire format is what a consumer's line parser reads,
// so its shape is pinned here: escaped label, id in parens, status, and a
// truncation marker only when the stored result is longer than the summary.
func TestNotification_WireShape(t *testing.T) {
	in := []Task{
		{ID: "t1", Label: `the "big" job`, Status: StatusCompleted, Summary: "all good", Result: "all good"},
		{ID: "t2", Label: "other", Status: StatusFailed, Summary: "short", Result: strings.Repeat("x", 900)},
		{ID: "t3", Label: "quiet", Status: StatusCancelled},
	}
	msg := DefaultNotifyFormatter(in)

	lines := strings.Split(strings.TrimPrefix(msg, NotificationPrefix), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3:\n%s", len(lines), msg)
	}
	if want := `Task "the 'big' job" (t1) completed. Result: all good`; lines[0] != want {
		t.Errorf("line 0 = %q\nwant     %q", lines[0], want)
	}
	if !strings.Contains(lines[1], "(t2) failed.") || !strings.Contains(lines[1], "[truncated — call task_status(t2) for the full result]") {
		t.Errorf("line 1 = %q, want failed + truncation marker", lines[1])
	}
	if want := `Task "quiet" (t3) cancelled.`; lines[2] != want {
		t.Errorf("line 2 = %q, want %q (no Result clause without a summary)", lines[2], want)
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
