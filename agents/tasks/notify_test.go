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
	// Three tasks, plus the retry hint the failed one earns, plus the guidance.
	if len(lines) != 5 {
		t.Fatalf("%d lines, want 5:\n%s", len(lines), msg)
	}
	if lines[4] != NotifyGuidance {
		t.Errorf("last line = %q, want the guidance", lines[4])
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

// The hint is its own line, and only when there is something to resume. Inside
// a task line it would be read as part of that task's result; on every
// notification it would be noise the model learns to skip.
func TestNotification_RetryHintIsItsOwnLineAndOnlyWhenFailed(t *testing.T) {
	const hint = "(task_retry can resume a failed task from where it stopped)"

	noneFailed := DefaultNotifyFormatter([]Task{
		{ID: "t1", Label: "a", Status: StatusCompleted, Summary: "ok"},
		{ID: "t2", Label: "b", Status: StatusCancelled},
	})
	if strings.Contains(noneFailed, hint) {
		t.Errorf("hint offered with nothing to resume:\n%s", noneFailed)
	}

	// Two failures still earn one hint: it names the tool, not the task.
	twoFailed := DefaultNotifyFormatter([]Task{
		{ID: "t1", Label: "a", Status: StatusFailed, Summary: "boom"},
		{ID: "t2", Label: "b", Status: StatusFailed, Summary: "bang"},
	})
	if n := strings.Count(twoFailed, hint); n != 1 {
		t.Errorf("hint appears %d times, want 1:\n%s", n, twoFailed)
	}
	lines := strings.Split(strings.TrimPrefix(twoFailed, NotificationPrefix), "\n")
	if got := lines[len(lines)-2]; got != hint {
		t.Errorf("line before the guidance = %q, want the hint alone", got)
	}
	for _, line := range lines[:len(lines)-2] {
		if strings.Contains(line, hint) {
			t.Errorf("hint leaked into a task line: %q", line)
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
