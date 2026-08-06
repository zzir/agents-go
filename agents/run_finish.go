package agents

import "context"

// usageSnapshot returns a detached copy of the run's usage accumulator for
// anything handed to the caller — a RunResult, a RunState, error details. The
// live accumulator keeps changing (parallel agent-as-tool runs fold in, a
// resumed run keeps adding), so what escapes the runner is always a copy of
// its own: reading it needs no synchronization, and a later resume can never
// mutate a result the caller already holds.
func (r *runner) usageSnapshot() *Usage {
	u := r.rc.Usage.Snapshot()
	return &u
}

// baseResult fills the fields every RunResult carries however the run ended:
// the input, the item log, the responses, the last agent, and the three
// accumulators. Each ending then adds only what makes it different —
// FinalOutput, StoppedEarly, Interruptions plus State.
//
// Every ending goes through here, so a field added to RunResult is added once
// rather than at four returns, where missing one leaves it silently absent
// from that ending alone.
//
// NewItems is the unfiltered log (r.sessionItems), never the loop's
// generatedItems: a handoff input filter and a mid-run recompaction reset the
// model's view, while the result reports what the run produced. It is nil for
// a fresh run that failed before producing anything; a resume seeds it from
// the paused state.
func (r *runner) baseResult() *RunResult {
	return &RunResult{
		Input:            r.state.originalInput,
		NewItems:         r.sessionItems,
		RawResponses:     r.state.rawResponses,
		LastAgent:        r.state.agent,
		Usage:            r.usageSnapshot(),
		GuardrailResults: r.snapshotGuardrailResults(),
		Diagnostics:      r.diagnostics.All(),
	}
}

// finishRun is the final-output tail shared by the normal completion path and
// a max-turns recovery. Order: the agent-end hook fires FIRST, before output
// guardrails, so a tripped guardrail does not suppress it; then output
// guardrails; then session persistence and compaction, so a guardrail-tripped
// final output is never persisted.
func (r *runner) finishRun(ctx context.Context, finalOutput any) (*RunResult, error) {
	agent := r.state.agent
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
	res := r.baseResult()
	res.FinalOutput = finalOutput
	// A run can also reach its final output on the very turn the stop was asked
	// for — a single-turn agent always does, and each Loop attempt starts at
	// turn one, so the loop's turn-boundary check never fires for them. The
	// flag answers "did the caller stop this", not "where did it stop", so it
	// is set wherever the request is live.
	res.StoppedEarly = r.ctrl.stopRequested()
	return res, nil
}

// recoverMaxTurns gives ErrorHandlers.MaxTurns a chance to turn a turn-budget
// overrun into a normal completion. It returns (nil, nil) when there is no
// handler or it declines — the caller then fails with the MaxTurnsError. On
// recovery the agent span still records the overrun, the synthesized fallback
// message joins the run's items and session unless the handler opted out, and
// the run finishes through the same guardrail/persist/hook tail as a normal
// final output.
func (r *runner) recoverMaxTurns(ctx context.Context, cause *MaxTurnsError) (*RunResult, error) {
	// Handlers see the session view of the run (never reset by handoff input
	// filters).
	rec, err := r.resolveErrorRecovery(ctx, "max_turns", r.opts.Exec.ErrorHandlers.MaxTurns, cause,
		r.state.agent, r.state.originalInput, r.sessionItems, r.state.rawResponses)
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
	return r.finishRun(ctx, rec.finalOutput)
}

// fail is how every failure inside the loop leaves it: the cause wrapped in a
// *RunError carrying the run's progress so far, so a caller reaches the
// completed turns through RunError.Result instead of getting a bare error.
func (r *runner) fail(err error) error {
	// Mark the current agent span failed so the error is visible in traces;
	// child spans (generation, function) set their own errors at the source.
	r.agentSpan.SetError(err.Error(), nil)
	return &RunError{Result: r.baseResult(), err: err}
}
