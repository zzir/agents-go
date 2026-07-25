package agents

import "context"

// TurnResult describes one completed turn: the model call and everything that
// followed from it.
//
// It is what the turn-boundary hooks are handed, so a caller can decide what
// happens next from what actually happened rather than from agent-level
// configuration set before the run began.
type TurnResult struct {
	// Turn is the 1-based turn number within the run.
	Turn int
	// Response is the model response this turn was built from.
	Response *ModelResponse
	// NewItems is everything the turn produced, in order: the model's own
	// output items followed by any tool outputs and handoff records.
	NewItems []RunItem
}

// ToolCallNames returns the names of the tools called this turn, in call order.
//
// It exists because "stop once tool X has been called" is the common reason to
// look at a turn at all, and writing that predicate by hand means knowing which
// item types carry a tool name.
func (tr *TurnResult) ToolCallNames() []string {
	var names []string
	for _, it := range tr.NewItems {
		if tc, ok := it.(*ToolCallItem); ok {
			if name := tc.FunctionCall().Name; name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// stopAfterTurn asks ExecOptions.ShouldStopAfterTurn whether the run ends here,
// and derives the final output when it does.
//
// It is called at the turn boundary — after the turn's items are persisted,
// before the next model call — so a run that stops here has its full history
// saved and needs no unwinding.
func (r *runner) stopAfterTurn(ctx context.Context, agent *Agent, turn int, resp *ModelResponse, items []RunItem) (bool, any, error) {
	hook := r.opts.Exec.ShouldStopAfterTurn
	if hook == nil {
		return false, nil, nil
	}
	stop, err := hook(ctx, &TurnResult{Turn: turn, Response: resp, NewItems: items})
	if err != nil || !stop {
		return false, nil, err
	}
	return true, turnFinalOutput(agent, items), nil
}

// turnFinalOutput is what a run stopped at a turn boundary reports as its final
// output: the turn's last message if it produced one, otherwise its last tool
// output.
//
// A turn that ran tools and stopped has no closing message — the model never
// got to write one — so falling through to the tool result is what makes the
// result useful rather than empty. The full turn is on RunResult.NewItems for
// anything more specific.
func turnFinalOutput(agent *Agent, items []RunItem) any {
	if m := lastMessageItem(items); m != nil {
		if text := m.Text(); text != "" {
			return text
		}
	}
	for i := len(items) - 1; i >= 0; i-- {
		if out, ok := items[i].(*ToolCallOutputItem); ok {
			return coerceToolFinalOutput(agent, out.Output)
		}
	}
	return ""
}
