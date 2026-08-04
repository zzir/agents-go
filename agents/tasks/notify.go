package tasks

import (
	"fmt"
	"strings"
)

// NotificationPrefix marks a task-completion message injected into a parent
// session.
//
// The notification is a USER-role entry: the model reads it verbatim, which is
// the point — it is news the model has to act on. A UI must nonetheless render
// it as a notification card rather than a user bubble, because nobody typed it.
const NotificationPrefix = "[task-notification] "

// DefaultNotifyFormatter renders one line per finished task.
//
// One wake-up carries every pending task, batched deliberately: a dozen tasks
// finishing together would otherwise mean a dozen runs, each restating the
// others' news.
//
// The line carries the SUMMARY, not the result. A task returning ten thousand
// words must not paste them into the parent's context to say it is done; the
// truncation marker tells the model where the rest is.
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
	return NotificationPrefix + strings.Join(lines, "\n")
}

// notifyEscape flattens text onto the single line the wire format is: one
// task, one line. A label and a summary are model output — untrusted — and an
// embedded newline would let them mint LINES of their own: a consumer parsing
// per line would read a multi-line result as its first line plus garbage, and
// a crafted result containing "Task \"x\" (id) completed." forges a whole
// notification card for a task the sender does not own.
func notifyEscape(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	// Quotes too, not just newlines: the label is delimited by them, and a
	// greedy label match would let a summary containing
	// `" (t-forged) completed. Result: x` re-aim the parsed id and status
	// on the SAME line, no newline needed. Stripping the delimiter from
	// untrusted text is what actually closes the forgery.
	return strings.ReplaceAll(s, `"`, "'")
}
