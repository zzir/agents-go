package tasks

import (
	"fmt"
	"slices"
	"strings"
)

// NotificationPrefix marks a task-completion message injected into a parent
// session. The notification is a USER-role entry the model reads verbatim, but
// a UI must render it as a notification card, not a user bubble — nobody typed it.
const NotificationPrefix = "[task-notification] "

// NotifyGuidance is the last line of every notification: what the woken
// parent is to DO with it. Without it a diligent model redoes the finished work.
const NotifyGuidance = "(Tell the person what happened. The work above is done — do not repeat or re-check it unless they ask.)"

// DefaultNotifyFormatter renders one line per finished task. One wake-up carries
// every pending task, batched so a dozen finishing together do not mean a dozen
// runs. Each line carries the SUMMARY, not the full result; the truncation
// marker tells the model where the rest is. The guidance closes it.
func DefaultNotifyFormatter(ts []Task) string {
	lines := make([]string, 0, len(ts))
	for i := range ts {
		t := &ts[i]
		line := fmt.Sprintf("Task %q (%s) %s.", notifyEscape(t.Label), t.ID, t.Status)
		if t.Summary != "" {
			line += " Result: " + notifyEscape(t.Summary)
			if len(t.Result) > len(t.Summary) {
				line += fmt.Sprintf(" [truncated — call task_status(%s) for the full result]", t.ID)
			}
		}
		lines = append(lines, line)
	}
	// The retry hint is its OWN line: a task line is a machine-readable record,
	// and text inside one would read as part of the result.
	if slices.ContainsFunc(ts, func(t Task) bool { return t.Status == StatusFailed }) {
		lines = append(lines, "(task_retry can resume a failed task from where it stopped)")
	}
	lines = append(lines, NotifyGuidance)
	return NotificationPrefix + strings.Join(lines, "\n")
}

// notifyEscape flattens untrusted text onto the one line the wire format is;
// a newline or a quote would let a label forge another task's line (spec §2.13).
func notifyEscape(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	// Quotes too: the label is delimited by them.
	return strings.ReplaceAll(s, `"`, "'")
}
