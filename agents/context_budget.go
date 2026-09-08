package agents

import (
	"context"
	"fmt"
	"slices"
)

// ContextBudget tells the model how much of its context window is in use.
// InputFilter appends the figure as the last input item of every model call
// and leaves the instructions alone, so a cached prefix stays cached (spec §2.5i).
type ContextBudget struct {
	// Window is the model's context window in tokens. Zero sends nothing.
	Window int
	// WindowFor, when set, answers the active agent's window, so a handoff
	// to an agent on another model reports that one; a zero answer falls
	// back to Window.
	WindowFor func(agent *Agent) int
	// Occupied is what the last model call before this run measured, input
	// and output together. Zero means unknown: the run's first call then
	// carries no figure, and every later call uses the run's own usage.
	Occupied int64
}

// InputFilter returns the CallModelInputFilter that appends the notice.
func (b ContextBudget) InputFilter() CallModelInputFilter {
	return func(_ context.Context, rc *RunContext, agent *Agent, data ModelInputData) (ModelInputData, error) {
		window := b.Window
		if b.WindowFor != nil {
			if w := b.WindowFor(agent); w > 0 {
				window = w
			}
		}
		used := b.used(rc)
		if window <= 0 || used <= 0 || serverManaged(rc) {
			return data, nil
		}
		data.Input = append(slices.Clone(data.Input), InputItemsFromSystemText(notice(used, window))...)
		return data, nil
	}
}

// serverManaged reports a run whose history lives with the provider: only
// new items go on the wire and the provider keeps them, so a notice per call
// would pile up in the thread rather than replace the last one.
func serverManaged(rc *RunContext) bool {
	if rc == nil || rc.inheritedOpts == nil {
		return false
	}
	conv := rc.inheritedOpts.Conversation
	return conv.UsePreviousResponseID || conv.ConversationID != ""
}

// used is the newest measured figure: the run's last request, else Occupied.
func (b ContextBudget) used(rc *RunContext) int64 {
	if rc != nil && rc.Usage != nil {
		if u := rc.Usage.Snapshot(); len(u.RequestUsageEntries) > 0 {
			last := u.RequestUsageEntries[len(u.RequestUsageEntries)-1]
			return last.InputTokens + last.OutputTokens
		}
	}
	return b.Occupied
}

// notice renders the one line the model reads (format: spec §4).
func notice(used int64, window int) string {
	left := max(100-used*100/int64(window), 0)
	return fmt.Sprintf("Context budget: about %d of %d tokens in use (%d%% left).", used, window, left)
}
