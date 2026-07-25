package agents

import "context"

// TurnSnapshot is everything a turn was resolved to, captured before the model
// is called.
//
// The resolution used to be inline in the loop: a dozen locals, each computed
// from the agent and the run context, read again a hundred lines later. Naming
// the result makes the turn's configuration a value — one a hook can inspect,
// and one a hook can replace so the NEXT turn runs differently without the
// agent having been mutated underneath a concurrent run.
type TurnSnapshot struct {
	Agent        *Agent
	Model        Model
	Settings     *ModelSettings
	Instructions string
	Prompt       *Prompt
	Tools        []Tool
	Handoffs     []Handoff
	OutputSchema OutputSchema
	// Input is what the model is sent this turn. Under server-managed
	// conversation state that is the new items only, not the whole history.
	//
	// The RUNNER owns this field. A snapshot returned from PrepareNextTurn has
	// it replaced with the next turn's real input, because a prepared snapshot
	// is nearly always a copy of the previous turn's and honoring its Input
	// would replay that turn with the tool call and its output missing. To edit
	// what a call sends, use ModelOptions.InputFilter, which runs per turn and
	// sees the input the loop actually built.
	Input []TResponseInputItem
}

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
	// Snapshot is what the turn was resolved to before it ran.
	Snapshot *TurnSnapshot
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
func (r *runner) stopAfterTurn(ctx context.Context, agent *Agent, turn int, resp *ModelResponse, snap *TurnSnapshot, items []RunItem) (bool, any, error) {
	hook := r.opts.Exec.ShouldStopAfterTurn
	if hook == nil {
		return false, nil, nil
	}
	stop, err := hook(ctx, &TurnResult{Turn: turn, Response: resp, NewItems: items, Snapshot: snap})
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

// buildSnapshot resolves everything a turn needs before the model is called.
//
// Each of these can fail — dynamic instructions, a prompt callback, a tool's
// enable predicate — and each failure is the run's, so they are resolved
// together rather than scattered through the turn body.
func (r *runner) buildSnapshot(ctx context.Context, agent *Agent, input []TResponseInputItem) (*TurnSnapshot, error) {
	model, err := r.resolveModel(agent)
	if err != nil {
		return nil, err
	}
	instructions, err := agent.GetSystemPrompt(ctx, r.rc)
	if err != nil {
		return nil, err
	}
	prompt, err := agent.GetPrompt(ctx, r.rc)
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
	NewItems []RunItem
}

// savePointResult is what the save point decided.
type savePointResult struct {
	// Stop ends the run with FinalOutput.
	Stop        bool
	FinalOutput any
	// Recompacted reports that Input replaces the run's context.
	Recompacted bool
	Input       []TResponseInputItem
	// NextSnapshot, when non-nil, is used for the next turn instead of
	// resolving one from the agent.
	NextSnapshot *TurnSnapshot
}

// savePoint is the turn boundary: the point at which the turn's assistant
// message and every tool result are persisted, and the next model call has not
// happened yet.
//
// It is one function because the order of these steps is the contract, and
// scattering them through the loop is how that order gets quietly broken:
//
//  1. flush the turn to the session
//  2. ask whether the run should stop
//  3. compact, rebuilding the context from the log
//  4. let a hook prepare the next turn
//
// Persisting first is what makes the rest safe: a run that stops at step 2, or
// whose context is rewritten at step 3, has its history already written.
// Deciding to stop before compacting means the decision is made against the
// turn that actually happened, not a shortened view of it.
func (r *runner) savePoint(ctx context.Context, in savePointInput) (savePointResult, error) {
	var out savePointResult

	r.ctrl.setPhase(PhasePersisting)
	if err := r.persistSessionItems(ctx); err != nil {
		return out, err
	}

	tr := &TurnResult{Turn: in.Turn, Response: in.Response, NewItems: in.NewItems, Snapshot: in.Snapshot}

	stop, final, err := r.stopAfterTurn(ctx, in.Agent, in.Turn, in.Response, in.Snapshot, in.NewItems)
	if err != nil {
		return out, err
	}
	if stop {
		out.Stop, out.FinalOutput = true, final
		return out, nil
	}

	// Compact mid-run. A run that calls thirty tools overruns its context
	// window long before the run-level pass would look.
	r.ctrl.setPhase(PhaseCompaction)
	compacted, did, err := r.recompactAtSavePoint(ctx)
	if err != nil {
		return out, err
	}
	out.Recompacted, out.Input = did, compacted

	if prepare := r.opts.Exec.PrepareNextTurn; prepare != nil {
		next, perr := prepare(ctx, tr)
		if perr != nil {
			return out, perr
		}
		out.NextSnapshot = next
	}
	return out, nil
}
