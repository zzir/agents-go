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
	// Occupied is what the last model call before this run measured, input
	// and output together. Zero means unknown: the run's first call then
	// carries no figure, and every later call uses the run's own usage.
	Occupied int64
}

// InputFilter returns the CallModelInputFilter that appends the notice.
func (b ContextBudget) InputFilter() CallModelInputFilter {
	return func(_ context.Context, rc *RunContext, _ *Agent, data ModelInputData) (ModelInputData, error) {
		used := b.used(rc)
		if b.Window <= 0 || used <= 0 {
			return data, nil
		}
		data.Input = append(slices.Clone(data.Input), InputItemsFromSystemText(b.notice(used))...)
		return data, nil
	}
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
func (b ContextBudget) notice(used int64) string {
	left := max(100-used*100/int64(b.Window), 0)
	return fmt.Sprintf("Context budget: about %d of %d tokens in use (%d%% left).", used, b.Window, left)
}
