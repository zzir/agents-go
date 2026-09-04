package agents

import (
	"context"
	"errors"

	"github.com/zzir/agents-go/tracing"
)

// errServerManagedReplace is a Replace verdict's failure under server-managed
// history, which cannot be rewritten (spec §2.6).
const errServerManagedReplace = "input guardrail replacement cannot apply: the conversation is server-managed " +
	"and its history cannot be rewritten; use a locally-managed session, or Trip instead of Replace"

// inputGuardOutcome carries the racing input guardrails' results and
// tripwire/error off their goroutine.
type inputGuardOutcome struct {
	results []GuardrailResult
	err     error
}

// inputGuardRace is the non-blocking input guardrails racing the first model
// call: ch delivers their verdict, and modelCtx is what the raced call runs under.
type inputGuardRace struct {
	ch          chan inputGuardOutcome
	modelCtx    context.Context
	cancelModel context.CancelFunc
	cancelGuard context.CancelFunc
}

// stop cancels the in-flight guardrails and the raced call's context.
// Nil-safe and idempotent, so the loop defers it unconditionally.
func (g *inputGuardRace) stop() {
	if g == nil {
		return
	}
	g.cancelGuard()
	g.cancelModel()
}

// inputGateResult is what the first turn's gate produced: original (possibly
// Replaced), model (the rebuilt turn input, or nil) and race (or nil).
type inputGateResult struct {
	original []InputItem
	model    []InputItem
	race     *inputGuardRace
}

// firstTurnInputGuardrails runs the input stage ahead of the first model call:
// Blocking guardrails gate it (a Replace rebuilds the input), the rest race it (spec §2.6).
func (r *runner) firstTurnInputGuardrails(
	ctx context.Context,
	startAgent *Agent,
	originalInput []InputItem,
	usedOriginalInput bool,
	snapshot *TurnSnapshot,
	rebuild func(replaced []InputItem) ([]InputItem, error),
) (inputGateResult, error) {
	out := inputGateResult{original: originalInput}
	all := selectStage(r.runGuardrails(startAgent), StageInput)
	var blocking, parallel []Guardrail
	for _, g := range all {
		if g.Blocking {
			blocking = append(blocking, g)
		} else {
			parallel = append(parallel, g)
		}
	}

	// Blocking guardrails gate the call: a tripwire prevents it.
	if len(blocking) > 0 {
		gspan := r.trace.StartGuardrailSpan("input", r.agentParentID())
		res, gerr := runStageConcurrent(ctx, r.rc, blocking,
			GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: out.original})
		r.recordGuardrailResults(res...)
		if repl, ok := inputReplacement(res); ok {
			// Replace means the model must see the replacement: rebuild THIS
			// turn's input from it, or the model gets the original.
			if !usedOriginalInput {
				// A server-managed turn sends only a delta; the history the
				// replacement would rewrite lives on the server (spec §2.6).
				rerr := NewUserError(errServerManagedReplace)
				gspan.SetError(rerr.Error(), nil)
				gspan.Finish()
				return out, rerr
			}
			out.original = repl
			rebuilt, rerr := rebuild(repl)
			if rerr != nil {
				gspan.SetError(rerr.Error(), nil)
				gspan.Finish()
				return out, rerr
			}
			out.model = rebuilt
			snapshot.Input = rebuilt
			r.rc.setTurnInput(rebuilt)
		}
		if gerr != nil {
			gspan.SetError(gerr.Error(), nil)
			gspan.Finish()
			return out, gerr
		}
		gspan.Finish()
	}

	// Non-blocking guardrails race the call: a tripwire cancels it; a Replace
	// misses the call it raced and applies from the next turn on (spec §2.6).
	if len(parallel) > 0 {
		gctx, gcancel := context.WithCancel(ctx)
		modelCtx, modelCancel := context.WithCancel(ctx)
		race := &inputGuardRace{ch: make(chan inputGuardOutcome, 1), modelCtx: modelCtx, cancelModel: modelCancel, cancelGuard: gcancel}
		parentID := r.agentParentID() // read before the goroutine races a handoff
		payloadInput := out.original
		go func() {
			gspan := r.trace.StartGuardrailSpan("input", parentID)
			res, gerr := runStageConcurrent(gctx, r.rc, parallel,
				GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: payloadInput})
			if gerr != nil {
				gspan.SetError(gerr.Error(), nil)
				// The verdict stops the raced call; delivered on ch, it outranks
				// whatever the call then returns.
				modelCancel()
			}
			gspan.Finish()
			race.ch <- inputGuardOutcome{results: res, err: gerr}
		}()
		out.race = race
	}
	return out, nil
}

// racedCallOutcome is what a raced first-turn model call produced. On failure
// exactly one of stopped, guardErr or modelErr is set; guardErr outranks the model.
type racedCallOutcome struct {
	resp     *ModelResponse
	original []InputItem // possibly rewritten by a Replace verdict
	guardErr error
	modelErr error
	stopped  bool
}

// raceModelCall runs the first turn's model call under race.modelCtx, on THIS
// goroutine in its streamed form; a tripwire discards the model outcome (spec §2.6).
func (r *runner) raceModelCall(span *tracing.SpanHandle, call func(context.Context) (*ModelResponse, error), race *inputGuardRace, originalInput []InputItem) racedCallOutcome {
	out := racedCallOutcome{original: originalInput}
	var err error
	out.resp, err = call(race.modelCtx)
	// An abandoned stream stops the run where it stands (spec §2.0): cancel the
	// guardrails and leave rather than wait on a verdict nobody will read.
	if r.closed.Load() || errors.Is(err, errConsumerStopped) {
		race.stop()
		span.Finish()
		out.resp, out.stopped = nil, true
		return out
	}
	// A call that failed on its own cancels the race first, so a slow guardrail
	// cannot hold a failed run open; a delivered verdict still wins (spec §2.6).
	if err != nil {
		race.stop()
	}
	g := <-race.ch
	r.recordGuardrailResults(g.results...)
	if repl, ok := inputReplacement(g.results); ok {
		if r.opts.Conversation.UsePreviousResponseID || r.opts.Conversation.ConversationID != "" {
			// Server-managed turns send only deltas: the replacement would apply to nothing.
			span.Finish()
			out.resp = nil
			out.guardErr = NewUserError(errServerManagedReplace)
			return out
		}
		// Too late for the call it raced; later turns and the
		// result see the replacement.
		out.original = repl
	}
	// A verdict outranks the model outcome — but the guardrails' own
	// cancellation after the model failed is race.stop() honored, not a verdict.
	if g.err != nil && (err == nil || !errors.Is(g.err, context.Canceled)) {
		span.SetError(g.err.Error(), nil)
		span.Finish()
		out.resp = nil
		out.guardErr = g.err
		return out
	}
	out.modelErr = err
	return out
}

// recordGuardrailResults appends guardrail results under the lock, since
// concurrent tool calls record in parallel.
func (r *runner) recordGuardrailResults(res ...GuardrailResult) {
	if len(res) == 0 {
		return
	}
	r.guardrailMu.Lock()
	r.guardrailResults = append(r.guardrailResults, res...)
	r.guardrailMu.Unlock()
}

// snapshotGuardrailResults copies the accumulated results for a RunResult.
func (r *runner) snapshotGuardrailResults() []GuardrailResult {
	r.guardrailMu.Lock()
	defer r.guardrailMu.Unlock()
	return append([]GuardrailResult(nil), r.guardrailResults...)
}

// runGuardrails is the run-level guardrail set for an agent: run-scoped ones
// first, then the agent's own.
func (r *runner) runGuardrails(agent *Agent) []Guardrail {
	if len(r.opts.Guardrails) == 0 {
		return agent.Guardrails
	}
	out := make([]Guardrail, 0, len(r.opts.Guardrails)+len(agent.Guardrails))
	out = append(out, r.opts.Guardrails...)
	out = append(out, agent.Guardrails...)
	return out
}
