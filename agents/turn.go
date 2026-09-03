package agents

import (
	"context"
	"slices"

	"github.com/zzir/agents-go/agents/session"
)

// TurnSnapshot is everything a turn was resolved to, captured before the model
// is called. Naming the turn's configuration as a value lets a hook inspect it,
// and replace it so the NEXT turn runs differently without mutating the agent
// underneath a concurrent run.
type TurnSnapshot struct {
	Agent        *Agent
	Model        Model
	Settings     *ModelSettings
	Instructions string
	Prompt       *Prompt
	Tools        []*Tool
	Handoffs     []Handoff
	OutputSchema OutputSchema
	// Input is what the model is sent this turn — under server-managed state
	// the new items only. The RUNNER owns it: a snapshot from PrepareNextTurn
	// has it replaced with the next turn's real input. To edit what a call
	// sends, use ModelOptions.InputFilter.
	Input []InputItem
}

// TurnResult describes one completed turn: the model call and everything that
// followed. It is what the turn-boundary hooks are handed; each hook sees the
// turn as it happened, and assigning to a field of its value reaches neither
// the run nor the next hook (spec §2.3c).
type TurnResult struct {
	// Turn is the 1-based turn number within the run.
	Turn int
	// Response is the model response this turn was built from.
	Response *ModelResponse
	// NewItems is everything the turn produced, in order: the model's own
	// output items followed by any tool outputs and handoff records.
	NewItems []*RunItem
	// Snapshot is what the turn was resolved to before it ran.
	Snapshot *TurnSnapshot
}

// ToolCallNames returns the names of the tools called this turn, in call order.
// It saves a caller writing "stop once tool X was called" by hand.
func (tr *TurnResult) ToolCallNames() []string {
	var names []string
	for _, it := range tr.NewItems {
		if it.Kind == ItemToolCall {
			if name := it.FunctionCall().Name; name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// stopAfterTurn asks ExecOptions.ShouldStopAfterTurn whether the run ends here
// and derives the final output; the hook gets its own copy — spec §2.3c.
func (r *runner) stopAfterTurn(ctx context.Context, agent *Agent, tr *TurnResult) (bool, any, error) {
	hook := r.opts.Exec.ShouldStopAfterTurn
	if hook == nil {
		return false, nil, nil
	}
	forHook := *tr
	stop, err := hook(ctx, &forHook)
	if err != nil || !stop {
		return false, nil, err
	}
	return true, turnFinalOutput(agent, tr.NewItems), nil
}

// turnFinalOutput is what a run stopped at a turn boundary reports: the
// turn's last message if it produced one, else its last tool output.
func turnFinalOutput(agent *Agent, items []*RunItem) any {
	if m := lastMessageItem(items); m != nil {
		if text := m.Text(); text != "" {
			return text
		}
	}
	for _, item := range slices.Backward(items) {
		if item.Kind == ItemToolCallOutput {
			return coerceToolFinalOutput(agent, item.Output)
		}
	}
	return ""
}

// buildSnapshot resolves everything a turn needs before the model is called;
// each resolution can fail, and each failure is the run's.
func (r *runner) buildSnapshot(ctx context.Context, agent *Agent, input []InputItem) (*TurnSnapshot, error) {
	model, err := r.resolveModel(agent)
	if err != nil {
		return nil, err
	}
	instructions, err := agent.systemPrompt(ctx, r.rc)
	if err != nil {
		return nil, err
	}
	prompt, err := agent.resolvePrompt(ctx, r.rc)
	if err != nil {
		return nil, err
	}
	outputSchema := agentOutputSchema(agent)
	if err := outputSchemaError(outputSchema); err != nil {
		return nil, err
	}
	handoffs, err := r.enabledHandoffs(ctx, agent)
	if err != nil {
		return nil, err
	}
	tools, err := r.enabledTools(ctx, agent)
	if err != nil {
		return nil, err
	}
	return &TurnSnapshot{
		Agent:        agent,
		Model:        model,
		Settings:     r.resolveSettings(agent),
		Instructions: instructions,
		Prompt:       prompt,
		Tools:        tools,
		Handoffs:     handoffs,
		OutputSchema: outputSchema,
		Input:        input,
	}, nil
}

// savePointInput describes the turn that just finished.
type savePointInput struct {
	Turn     int
	Agent    *Agent
	Snapshot *TurnSnapshot
	Response *ModelResponse
	NewItems []*RunItem
}

// savePointResult is what the save point decided.
type savePointResult struct {
	// Stop ends the run with FinalOutput.
	Stop        bool
	FinalOutput any
	// Recompacted reports that Input replaces the run's context.
	Recompacted bool
	Input       []InputItem
	// NextSnapshot, when non-nil, is used for the next turn instead of
	// resolving one from the agent.
	NextSnapshot *TurnSnapshot
	// Injected is caller-supplied input to add before the next model call.
	Injected []*RunItem
}

// savePoint is the turn boundary: persist, ask to stop, compact, drain the
// injection queues, prepare the next turn — in that order; spec §2.3a.
func (r *runner) savePoint(ctx context.Context, in savePointInput) (savePointResult, error) {
	var out savePointResult

	if err := r.persistSessionItems(ctx); err != nil {
		return out, err
	}

	tr := &TurnResult{Turn: in.Turn, Response: in.Response, NewItems: in.NewItems, Snapshot: in.Snapshot}

	stop, final, err := r.stopAfterTurn(ctx, in.Agent, tr)
	if err != nil {
		return out, err
	}
	if stop {
		out.Stop, out.FinalOutput = true, final
		return out, nil
	}

	// Compact mid-run. A run that calls thirty tools overruns its context
	// window long before the run-level pass would look.
	compacted, did, err := r.recompactAtSavePoint(ctx)
	if err != nil {
		return out, err
	}
	out.Recompacted, out.Input = did, compacted

	// Injected input is drained after compaction so it is never folded away by
	// the pass that ran before it arrived.
	out.Injected = injectedInput(in.Agent, r.ctrl.takeTurnInput())

	if prepare := r.opts.Exec.PrepareNextTurn; prepare != nil {
		next, perr := prepare(ctx, tr)
		if perr != nil {
			return out, perr
		}
		out.NextSnapshot = next
	}
	return out, nil
}

// injectedInput turns caller-supplied input into run items, so every
// downstream path treats it like the input the run started with (spec §2.11b).
func injectedInput(agent *Agent, items []InputItem) []*RunItem {
	out := make([]*RunItem, 0, len(items))
	for _, item := range items {
		disp := ItemDisplay{Kind: DisplayMessage, Text: session.ItemText(item)}
		out = append(out, &RunItem{
			Kind:     ItemInjectedInput,
			Agent:    agent,
			Source:   Source{Type: SourceUser},
			RawInput: &item,
			display:  &disp,
		})
	}
	return out
}
