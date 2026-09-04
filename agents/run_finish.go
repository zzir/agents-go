package agents

import (
	"context"
	"errors"
)

// usageSnapshot returns a detached copy of the run's usage for anything handed
// to the caller, so a later resume cannot mutate a result already returned.
func (r *runner) usageSnapshot() *Usage {
	u := r.rc.Usage.Snapshot()
	return &u
}

// baseResult fills the fields every RunResult carries however the run ended.
// NewItems is the whole log (r.sessionItems), never the model's view of it.
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

// finishRun is the final-output tail: OnEnd first (a tripped guardrail does
// not suppress it), then output guardrails, then persistence and compaction.
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
		// Its own span: the likeliest place for a slow LLM-based guardrail, not
		// to be misreported under the turn's last phase.
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
	// The flag answers "did the caller stop this", not "where did it stop": a
	// run can reach its final output on the very turn the stop was asked for (spec §2.12).
	res.StoppedEarly = r.ctrl.stopRequested()
	return res, nil
}

// recoverMaxTurns gives ErrorHandlers.MaxTurns a chance to turn an overrun
// into a normal completion; (nil, nil) means no handler or it declined.
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

// fail wraps every in-loop failure in a *RunError carrying the run's progress,
// so a caller reaches the completed turns through RunError.Result.
func (r *runner) fail(err error) error {
	if errors.Is(err, errConsumerStopped) {
		// Not a failure: the consumer left, and nobody is told anything.
		return err
	}
	// Mark the current agent span failed so the error is visible in traces;
	// child spans (generation, function) set their own errors at the source.
	r.agentSpan.SetError(err.Error(), nil)
	return &RunError{Result: r.baseResult(), err: err}
}
