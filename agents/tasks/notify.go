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
// parent is to DO with it. Without it a diligent model redoes or re-verifies
// the finished work before reporting — a second run of the same tests, at
// the same cost — when the person wanted to be told.
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
	// The retry hint is its OWN line: a task line is a machine-readable record
	// consumers parse, and text appended inside one would read as part of the
	// result.
	if slices.ContainsFunc(ts, func(t Task) bool { return t.Status == StatusFailed }) {
		lines = append(lines, "(task_retry can resume a failed task from where it stopped)")
	}
	lines = append(lines, NotifyGuidance)
	return NotificationPrefix + strings.Join(lines, "\n")
}

// notifyEscape flattens text onto the one line the wire format is. A label and
// summary are untrusted model output: an embedded newline would let them mint
// lines of their own and forge a notification for a task the sender does not own.
func notifyEscape(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	// Quotes too: the label is delimited by them, so a summary containing a
	// quote could re-aim the parsed id and status on the same line without a
	// newline. Stripping the delimiter closes the forgery.
	return strings.ReplaceAll(s, `"`, "'")
}
