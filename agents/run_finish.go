package agents

import "context"

// finishRun is the final-output tail shared by the normal completion path and
// a max-turns recovery. Order: the agent-end hook fires FIRST (matching Python;
// before output guardrails — a tripped
// guardrail does not suppress on_agent_end), then output guardrails, then
// session persistence and compaction (kept after guardrails, matching Python's
// streaming path: a guardrail-tripped final output is not persisted).
func (r *runner) finishRun(ctx context.Context, agent *Agent, originalInput []TResponseInputItem, raw []*ModelResponse, finalOutput any) (*RunResult, error) {
	if agent.OnEnd != nil {
		if err := agent.OnEnd(ctx, r.rc, finalOutput); err != nil {
			return nil, err
		}
	}
	// Output guardrails: run-level ones first, then the producing agent's.
	// A Replace decision substitutes the final output and the run continues.
	if outGuardrails := selectStage(r.runGuardrails(agent), StageOutput); len(outGuardrails) > 0 {
		// The output stage is the likeliest place for a slow (LLM-based)
		// guardrail, and it ran under whatever phase the turn last set —
		// "model" or "tools" — which is exactly the misreport Phase exists to
		// prevent.
		r.ctrl.setPhase(PhaseGuardrails)
		gspan := r.trace.StartGuardrailSpan("output", r.agentParentID())
		res, gerr := runStageConcurrent(ctx, r.rc, outGuardrails,
			GuardrailPayload{Stage: StageOutput, Agent: agent, Output: finalOutput})
		r.recordGuardrailResults(res...)
		if gerr != nil {
			gspan.SetError(gerr.Error(), nil)
			gspan.Finish()
			return nil, gerr
		}
		gspan.Finish()
		for _, g := range res {
			if g.Decision.Action == GuardrailReplace {
				finalOutput = g.Decision.Message
				break
			}
		}
	}
	if err := r.persistSessionItems(ctx); err != nil {
		return nil, err
	}
	r.compactAfterRun(ctx)
	return &RunResult{
		Input: originalInput,
		// The unfiltered item log: a handoff input filter rewrites the model's
		// view (generatedItems) but never what the result reports (Python parity:
		// new_items = session_items).
		NewItems:         r.sessionItems,
		RawResponses:     raw,
		FinalOutput:      finalOutput,
		LastAgent:        agent,
		Usage:            r.rc.Usage,
		GuardrailResults: r.snapshotGuardrailResults(),
		Diagnostics:      r.diagnostics.All(),
		// A run can also reach its final output on the very turn the stop was
		// asked for — a single-turn agent always does, and each Loop attempt
		// starts at turn one, so the turn-boundary check above never fires for
		// them. The flag answers "did the caller stop this", not "where did it
		// stop", so it is set wherever the request is live.
		StoppedEarly: r.ctrl.stopRequested(),
	}, nil
}

// recoverMaxTurns gives ErrorHandlers.MaxTurns a chance to turn a turn-budget
// overrun into a normal completion. It returns (nil, nil) when there is no
// handler or it declines — the caller then fails with the MaxTurnsError. On
// recovery the agent span still records the overrun (Python parity: the error
// is traced even when handled), the synthesized fallback message joins the
// run's items and session unless the handler opted out, and the run finishes
// through the same guardrail/persist/hook tail as a normal final output.
func (r *runner) recoverMaxTurns(ctx context.Context, cause *MaxTurnsError, originalInput []TResponseInputItem, rawResponses []*ModelResponse, agent *Agent) (*RunResult, error) {
	// Handlers see the session view of the run (never reset by handoff input
	// filters), like Python's session_items-based RunErrorData for max_turns.
	rec, err := r.resolveErrorRecovery(ctx, "max_turns", r.opts.Exec.ErrorHandlers.MaxTurns, cause, agent, originalInput, r.sessionItems, rawResponses)
	if err != nil || rec == nil {
		return nil, err
	}
	r.agentSpan.SetError(cause.Error(), map[string]any{"max_turns": r.maxTurns})
	if rec.message != nil {
		// finishRun reports r.sessionItems as NewItems, so the synthesized
		// fallback message joins the run there (and the session).
		r.sessionItems = append(r.sessionItems, rec.message)
		if !r.emit(&RunItemStreamEvent{Name: runItemEventName(rec.message), Item: rec.message}) {
			return nil, errConsumerStopped
		}
	}
	return r.finishRun(ctx, agent, originalInput, rawResponses, rec.finalOutput)
}

func (r *runner) fail(err error, input []TResponseInputItem, items []RunItem, raw []*ModelResponse, last *Agent) error {
	// Mark the current agent span failed so the error is visible in traces;
	// child spans (generation, function) set their own errors at the source.
	r.agentSpan.SetError(err.Error(), nil)
	// Report the unfiltered item log when a handoff input filter has reset the
	// caller's generatedItems view (Python parity: RunErrorDetails carries
	// session_items). Without a filter the two are identical.
	newItems := items
	if len(r.sessionItems) > len(items) {
		newItems = r.sessionItems
	}
	details := &RunErrorDetails{
		Input:            input,
		NewItems:         newItems,
		RawResponses:     raw,
		LastAgent:        last,
		Usage:            r.rc.Usage,
		GuardrailResults: r.snapshotGuardrailResults(),
		Diagnostics:      r.diagnostics.All(),
	}
	var ae *AgentsError
	if asAgentsError(err, &ae) {
		ae.Details = details
		return err
	}
	return err
}
