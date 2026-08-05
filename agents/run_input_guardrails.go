package agents

import (
	"context"
	"errors"

	"github.com/zzir/agents-go/tracing"
)

// This file is the runner's input-guardrail machinery for the first turn:
// Blocking guardrails gate the model call, non-blocking ones race it. The
// semantics live in spec.md §2.6; the loop in run.go only sequences the two.

// inputGuardOutcome carries the parallel input guardrails' collected results and
// tripwire/error off their goroutine, so the main loop can both honor the
// tripwire and record every result on the RunResult.
type inputGuardOutcome struct {
	results []GuardrailResult
	err     error
}

// inputGuardRace is the in-flight non-blocking input guardrails racing the
// first model call: ch delivers their collected verdict, cancel stops them.
type inputGuardRace struct {
	ch     chan inputGuardOutcome
	cancel context.CancelFunc
}

// stop cancels the in-flight guardrails. Nil-safe and idempotent, so the loop
// can defer it unconditionally: every early exit (e.g. a failed model call)
// stops an LLM-based guardrail instead of letting it run to completion after
// the run has already returned.
func (g *inputGuardRace) stop() {
	if g != nil && g.cancel != nil {
		g.cancel()
	}
}

// inputGateResult is what the first turn's guardrail gate produced. original
// is the run input, possibly rewritten by a Blocking guardrail's Replace
// verdict; model is the rebuilt turn input when that happened (nil otherwise);
// race carries any non-blocking guardrails spawned to race the model call
// (nil when there are none).
type inputGateResult struct {
	original []TResponseInputItem
	model    []TResponseInputItem
	race     *inputGuardRace
}

// firstTurnInputGuardrails runs the input stage ahead of the first model call:
// run-level guardrails first, then the starting agent's.
//
// A guardrail with Blocking=true runs to completion here — a gate. It runs
// after the turn's input is built and published, so it can inspect
// rc.TurnInput(); a Replace verdict rebuilds that input from the replacement
// (via rebuild), which is what makes the verdict real for the guarded call
// itself. The rest are spawned to race the model call; the returned race
// delivers their results and tripwire.
//
// Errors are returned bare; the loop wraps them with fail() so the error
// details carry the loop's state. Even on error, result.original reports the
// input as the guardrails left it.
func (r *runner) firstTurnInputGuardrails(
	ctx context.Context,
	startAgent *Agent,
	originalInput []TResponseInputItem,
	usedOriginalInput bool,
	snapshot *TurnSnapshot,
	rebuild func(replaced []TResponseInputItem) ([]TResponseInputItem, error),
) (inputGateResult, error) {
	out := inputGateResult{original: originalInput}
	all := selectStage(r.runGuardrails(startAgent), StageInput)
	var sequential, parallel []Guardrail
	for _, g := range all {
		if g.Blocking {
			sequential = append(sequential, g)
		} else {
			parallel = append(parallel, g)
		}
	}

	// Sequential (blocking) guardrails: a tripwire prevents the model call.
	if len(sequential) > 0 {
		gspan := r.trace.StartGuardrailSpan("input", r.agentParentID())
		res, gerr := runStageConcurrent(ctx, r.rc, sequential,
			GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: out.original})
		r.recordGuardrailResults(res...)
		if repl, ok := inputReplacement(res); ok {
			// The whole point of Replace: the model must see the
			// replacement. Rebuild THIS turn's input from it — leaving
			// the already-built input in place sent the original to the
			// model while the result claimed it was replaced.
			if !usedOriginalInput {
				// A server-managed turn (previous_response_id / a
				// server-held conversation) sends only a delta; the
				// history the replacement would rewrite lives on the
				// server and cannot be rewritten from here. Proceeding
				// would send the original while claiming otherwise —
				// fail instead.
				rerr := NewUserError(
					"input guardrail replacement cannot apply: the conversation is server-managed and its history cannot be rewritten; use a locally-managed session, or Trip instead of Replace")
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

	// Non-blocking guardrails race the model call: a tripwire cancels it,
	// so the run fails without waiting for a response nobody will use.
	// A Replace verdict from a racing guardrail necessarily misses the
	// call it raced — the request is already in flight when the verdict
	// lands — and applies from the next turn on; a guardrail that must
	// rewrite what the model sees sets Blocking.
	if len(parallel) > 0 {
		gctx, gcancel := context.WithCancel(ctx)
		race := &inputGuardRace{ch: make(chan inputGuardOutcome, 1), cancel: gcancel}
		parentID := r.agentParentID() // read before the goroutine races a handoff
		payloadInput := out.original
		go func() {
			gspan := r.trace.StartGuardrailSpan("input", parentID)
			res, gerr := runStageConcurrent(gctx, r.rc, parallel,
				GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: payloadInput})
			if gerr != nil {
				gspan.SetError(gerr.Error(), nil)
			}
			gspan.Finish()
			race.ch <- inputGuardOutcome{results: res, err: gerr}
		}()
		out.race = race
	}
	return out, nil
}

// racedCallOutcome is what a first-turn model call raced by input guardrails
// produced. On failure exactly one of the fields below is set:
//
//   - stopped: the consumer stopped ranging mid-call; the loop unwinds with
//     errConsumerStopped and nothing is reported.
//   - guardErr: a guardrail verdict — a tripwire, a guardrail failure, or a
//     Replace that cannot apply. It takes priority over the model outcome it
//     raced (which is discarded), and it must never enter overflow recovery.
//   - modelErr: the model call's own error, still eligible for the loop's
//     overflow compact-and-retry.
type racedCallOutcome struct {
	resp     *ModelResponse
	original []TResponseInputItem // possibly rewritten by a Replace verdict
	guardErr error
	modelErr error
	stopped  bool
}

// raceModelCall runs the first turn's model call with the non-blocking input
// guardrails watching the verdict from the side, so a tripwire cancels the
// in-flight call. The call itself stays on THIS goroutine in its usual form —
// streamed when the run streams — because racing must not de-stream it:
// yielding raw events from a helper goroutine would race the iterator, and
// downgrading to a blocking call silently cost a streaming UI its whole first
// turn of deltas.
//
// A tripped guardrail aborts the turn WITHOUT billing usage or firing OnLLMEnd
// — the model outcome is discarded. Raw events already yielded by a streamed
// call stand; the run's error is what says they came to nothing.
func (r *runner) raceModelCall(ctx context.Context, span *tracing.SpanHandle, model Model, req ModelRequest, race *inputGuardRace, originalInput []TResponseInputItem) racedCallOutcome {
	out := racedCallOutcome{original: originalInput}
	modelCtx, modelCancel := context.WithCancel(ctx)
	relay := make(chan inputGuardOutcome, 1)
	go func() {
		g := <-race.ch
		if g.err != nil {
			modelCancel()
		}
		relay <- g
	}()
	var err error
	if r.rawEvents {
		out.resp, err = r.streamOneModelCall(modelCtx, span, model, req)
	} else {
		out.resp, err = model.GetResponse(modelCtx, req)
	}
	// An abandoned stream must stop the run WHERE IT STANDS
	// (spec §2.0), and the guardrails are the run: waiting for
	// them here parked the consumer's `break` for a slow
	// guardrail's full duration — forever, for one that returns
	// only on cancellation, since the deferred cancel cannot run
	// while this read blocks. Cancel them and leave; their
	// verdict is about a turn nobody will read.
	if r.consumerStopped.Load() || errors.Is(err, errConsumerStopped) {
		race.stop()
		modelCancel()
		span.Finish()
		out.resp, out.stopped = nil, true
		return out
	}
	// The guardrails always finish — a tripwire cancelled the call,
	// completion just outlasted it — so honor a verdict still in
	// flight before trusting the model outcome. A call that failed on
	// its own is done deciding, though: cancel the race first, so a
	// slow guardrail cannot hold an already-failed run open. A verdict
	// already delivered still wins below; stop is idempotent.
	if err != nil {
		race.stop()
	}
	g := <-relay
	modelCancel()
	r.recordGuardrailResults(g.results...)
	if repl, ok := inputReplacement(g.results); ok {
		if r.opts.Conversation.UsePreviousResponseID || r.opts.Conversation.ConversationID != "" {
			// "Applies from the next turn on" is impossible here:
			// server-managed turns send only deltas and never
			// rebuild from the input, so the replacement would
			// apply to NOTHING while the result claimed
			// otherwise. Fail rather than pretend.
			span.Finish()
			out.resp = nil
			out.guardErr = NewUserError(
				"input guardrail replacement cannot apply: the conversation is server-managed and its history cannot be rewritten; use a locally-managed session, or Trip instead of Replace")
			return out
		}
		// Too late for the call it raced; later turns and the
		// result see the replacement.
		out.original = repl
	}
	// A guardrail verdict outranks the model outcome it raced — but a
	// cancellation error after the model already failed is not a verdict,
	// it is the race.stop() above being honored. Reporting it would mask
	// the model's own error behind "context canceled".
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
