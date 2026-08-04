package agents

// This file is the run loop and its entry points. The stages the loop
// sequences live in their own files: run_options.go (what a run can be told),
// run_prepare.go (input normalization, trace/session wiring, resume seeding),
// run_input_guardrails.go (the first-turn gate and race),
// run_server_cursor.go (server-managed history deltas), run_step.go (tool
// execution and side effects), run_persist.go (session writes),
// run_finish.go (completion and failure), run_resolve.go (per-turn
// model/tool/handoff resolution) and run_tracing.go (span recording).

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/zzir/agents-go/tracing"
)

// Run starts an agent run and returns it as a stream plus a control handle.
// Nothing executes until the stream is ranged: the run happens on the
// consumer's goroutine, one event at a time.
//
// input may be a string or a []TResponseInputItem; use InputItemsFromText for
// the common single-message case.
//
//	stream, ctrl := agents.Run(ctx, agent, "hi", agents.RunOptions{})
//	for ev, err := range stream {
//	    if err != nil { return err }
//	    ...
//	}
//
// For the result and nothing else, use RunSync — it also skips streaming the
// model call, which Run needs in order to forward raw events.
//
// Abandoning the stream stops the run where it stands. To stop cleanly at a
// turn boundary, call ctrl.StopAfterTurn and keep ranging until the stream
// ends.
//
// One exception to "the consumer's goroutine": a tool streaming partial
// results yields its ToolProgressEvent from the tool's own goroutine. Yields
// are serialized, so the loop body never runs concurrently with itself — but
// it does not always run on the goroutine that started the range. Code that
// pins work to that goroutine (a thread-locked UI, goroutine-local state)
// must hand such events off; see docs/streaming.md.
func Run(ctx context.Context, agent *Agent, input any, opts RunOptions) (RunStream, RunControl) {
	ctrl := newRunControl()
	return withMiddleware(ctx, agent, input, opts, ctrl, true), ctrl
}

// RunSync executes a run to completion and returns its result. It is Run
// without the stream: the model is called without streaming, and no raw model
// events are produced.
//
// It is the entry point to reach for unless you need to observe a run as it
// happens.
func RunSync(ctx context.Context, agent *Agent, input any, opts RunOptions) (*RunResult, error) {
	ctrl := newRunControl()
	return withMiddleware(ctx, agent, input, opts, ctrl, false).Collect()
}

// singleUse guards a RunStream against being ranged twice. A second range
// re-invokes the whole run — model billed again, tools re-executing their side
// effects, the session taking duplicate items — and it does so SILENTLY, which
// is how "break out of the loop, then Collect() for the result" duplicated a
// run. The second range yields an error instead.
func singleUse(stream RunStream) RunStream {
	var consumed atomic.Bool
	return func(yield func(StreamEvent, error) bool) {
		if !consumed.CompareAndSwap(false, true) {
			yield(nil, newUserError("run stream already consumed: a RunStream is single-use, and ranging it again would re-execute the run"))
			return
		}
		stream(yield)
	}
}

// withMiddleware builds the run's stream through the configured middleware
// chain. Input normalization happens once, up front, so a middleware inspects
// and edits the same item list the loop will use rather than a string it would
// have to normalize itself.
func withMiddleware(ctx context.Context, agent *Agent, input any, opts RunOptions, ctrl *runControl, rawEvents bool) RunStream {
	base := func(ctx context.Context, in RunInput) RunStream {
		return func(yield func(StreamEvent, error) bool) {
			runStream(ctx, in.Agent, in.Input, *in.Opts, ctrl, rawEvents, yield)
		}
	}
	return runViaMiddleware(ctx, agent, input, opts, base)
}

// runViaMiddleware is the one pipeline both entry points (fresh runs and
// resumes) share: normalize the input once, wrap base in the middleware chain
// — chainMiddleware of an empty list is base itself, so the no-middleware case
// needs no separate branch — and hand the result out as a single-use stream.
func runViaMiddleware(ctx context.Context, agent *Agent, input any, opts RunOptions, base RunFunc) RunStream {
	return singleUse(func(yield func(StreamEvent, error) bool) {
		items, err := normalizeInput(input)
		if err != nil {
			yield(nil, err)
			return
		}
		in := RunInput{Agent: agent, Input: items, Opts: &opts}
		for ev, ierr := range chainMiddleware(base, opts.Middlewares)(ctx, in) {
			if !yield(ev, ierr) {
				return
			}
		}
	})
}

// runStream is the body shared by Run and RunSync: prepare, loop, and report
// the outcome as the stream's terminal event or terminal error.
func runStream(ctx context.Context, agent *Agent, input any, opts RunOptions, ctrl *runControl, rawEvents bool, yield func(StreamEvent, error) bool) {
	r, modelInput, finishTrace, err := prepareRun(ctx, agent, input, opts)
	if err != nil {
		yield(nil, err)
		return
	}
	defer finishTrace()
	r.yield = yield
	r.ctrl = ctrl
	r.rawEvents = rawEvents

	// The sink rides on the context so code far from the loop — a model
	// decorator, a custom tool — can report trouble it recovered from.
	ctx = WithDiagnostics(ctx, r.diagnostics)
	// The run's own cancellation root: emit cancels it (cause
	// errConsumerStopped) when the consumer stops ranging, so in-flight work
	// stops instead of completing into a run nobody reads. The deferred
	// cancel also reels in anything a timed-out tool left running.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.cancelRun = cancel
	res, err := r.loop(ctx, agent, modelInput)
	r.finishStream(res, err)
}

// finishStream reports a completed loop to the consumer. A consumer that
// already stopped is told nothing further — yield has returned false, so there
// is nobody listening.
func (r *runner) finishStream(res *RunResult, err error) {
	r.ctrl.setPhase(PhaseIdle)
	// Settle the injection transaction by how the attempt ended: a completed
	// attempt delivered whatever it took (the session-less case, where no
	// persist ever commits); a failed or abandoned one returns its take so a
	// retrying middleware's next attempt delivers it instead of losing it.
	if err != nil || r.consumerStopped.Load() {
		r.ctrl.rollbackInjected()
	} else {
		r.ctrl.commitInjected()
	}
	if r.consumerStopped.Load() || errors.Is(err, errConsumerStopped) {
		return
	}
	if err != nil {
		r.yield(nil, err)
		return
	}
	r.yield(&RunCompletedEvent{Result: res}, nil)
}

// runner holds the mutable state for a single Run invocation.
type runner struct {
	opts      RunOptions
	rc        *RunContext
	maxTurns  int
	userInput []TResponseInputItem // the new input to persist to the session
	resume    *RunState            // non-nil when resuming from an interruption
	trace     *tracing.TraceHandle // non-nil when tracing is enabled
	agentSpan *tracing.SpanHandle  // current agent span, parent of generation/tool spans

	// yield delivers events to the consumer ranging the RunStream. It is
	// always set for a run started through Run or RunSync — there is no
	// "non-streaming mode" any more, only a consumer that discards.
	//
	// It returns false once the consumer stops ranging; emit records that in
	// consumerStopped and the loop unwinds through errConsumerStopped.
	yield func(StreamEvent, error) bool

	// ctrl is the handle the caller got back from Run: the graceful-stop flag,
	// the phase indicator, and the live agent/turn.
	ctrl *runControl

	// rawEvents asks for the model to be called through StreamResponse so its
	// raw events reach the consumer. RunSync leaves it false and gets a single
	// GetResponse call instead.
	//
	// This is the ONE remaining difference between the two entry points. It
	// used to be six, all keyed off a nil check on the streaming handle.
	rawEvents bool

	// consumerStopped is set by emit when the consumer stopped ranging. The
	// loop checks it wherever it checks for cancellation. Atomic because the
	// writer may be a tool goroutine (a progress emit under emitMu) while the
	// loop reads it lock-free — including after a timed-out tool left an
	// orphan goroutine behind.
	consumerStopped atomic.Bool

	// cancelRun cancels the run's context; emit calls it when the consumer
	// stops ranging, so work the loop is blocked on — an in-flight model call,
	// a running tool batch — is told to stop instead of completing into a run
	// nobody reads (spec §2.0: an abandoned stream stops the run where it
	// stands). The cause is errConsumerStopped.
	cancelRun context.CancelCauseFunc

	// sessionItems accumulates every generated item for session persistence.
	// Unlike the loop's generatedItems it is never reset by a handoff input
	// filter, so the session keeps the full conversation.
	sessionItems []RunItem

	// persistedSessionItems counts how many leading sessionItems have already
	// been written to the session. The loop persists incrementally — after each
	// turn and at an interruption — so a cancelled or failed run keeps every
	// completed turn instead of losing the whole run (matching Python's per-turn
	// save_result_to_session). Carried across interrupt/resume in RunState.
	persistedSessionItems int

	// userInputSaved guards the one-time persistence of userInput at loop start
	// so a per-turn save never rewrites it.
	userInputSaved bool

	// emitMu serializes yields. The run loop is not the only emitter: a tool
	// pushing progress does it from its own goroutine, and several tools run at
	// once.
	emitMu sync.Mutex

	// diagnostics collects trouble the run survived. Never nil.
	diagnostics *DiagnosticSink
	// diagnosticsSaved is how many diagnostics have already been attached to
	// entries, so each is recorded on the turn it happened in rather than on
	// every turn from then on.
	diagnosticsSaved int

	// log is the run's logger, already tagged with the run's identity. Never
	// nil — a disabled logger is a no-op rather than a check at every site.
	log *runLogger

	// lastUsage is the most recent model response's usage, held so the entries
	// that response produced can carry it.
	lastUsage *Usage

	// usagePending marks a model response whose usage has not yet landed on an
	// entry. It is cleared on attribution, so a turn persisted in two batches —
	// the split an approval pause creates — cannot count the same request
	// twice.
	usagePending bool

	// inputGuardrailsRan records that the first turn's input guardrails have
	// executed, so an overflow retry of that turn — which re-enters it with
	// the same turn number — does not run them again: a moderation guardrail
	// double-bills and double-fires its side effects, and a non-idempotent one
	// can trip on input it already passed.
	inputGuardrailsRan bool

	// injectedUpTo is the sessionItems length just after the latest injected
	// input was appended. A session write is allowed to commit the in-flight
	// injections only once it has persisted past this point — a persist whose
	// safe boundary stopped short of them must not mark them delivered.
	injectedUpTo int

	// disclosed names the deferred tools a ToolResult has opened up. It is
	// carried on RunState so a resumed run does not re-hide a tool the model
	// has already been told about.
	disclosed map[string]bool

	// consecutiveErrorTurns counts turns in a row where every tool call failed.
	// A turn with any success clears it; ToolLoopPolicy decides when enough is
	// enough.
	consecutiveErrorTurns int

	// toolsUsedBy tracks which agents have called tools this run, driving the
	// tool_choice reset (Agent.DisableToolChoiceReset). Keyed by agent name so
	// it can be carried across an interrupt/resume in RunState (Python
	// serializes its tool-use tracker snapshot), keeping the reset in effect for
	// every agent that used tools before the pause — not just the interrupted
	// one.
	toolsUsedBy map[string]bool

	// lastResponseID / lastStore record the final model call's response id and
	// store setting, used to drive session compaction after persistence.
	lastResponseID string
	lastStore      *bool

	// guardrailMu guards guardrailResults: the tool stages record from the
	// concurrent per-tool-call goroutines in runFunctionTools, while the input
	// and output stages record from the main loop.
	guardrailMu      sync.Mutex
	guardrailResults []GuardrailResult
}

// agentParentID returns the current agent span's ID for nesting child spans.
func (r *runner) agentParentID() string {
	if r.agentSpan == nil || r.agentSpan.Span == nil {
		return ""
	}
	return r.agentSpan.Span.SpanID
}

func (r *runner) loop(ctx context.Context, startAgent *Agent, originalInput []TResponseInputItem) (*RunResult, error) {
	// Finish the active agent span when the loop ends (nil-safe when untraced).
	defer func() { r.agentSpan.Finish() }()

	// inputRace holds the first turn's racing input guardrails once spawned.
	// The deferred stop covers every early exit (e.g. a failed model call), so
	// an LLM-based guardrail is cancelled instead of running to completion
	// after the run has already returned.
	var inputRace *inputGuardRace
	defer func() { inputRace.stop() }()

	// When resuming from an interruption, the seed carries the prior state and
	// the captured response, re-processed on the first iteration instead of
	// calling the model.
	seed := r.seedLoop(startAgent, originalInput)
	currentAgent := seed.agent
	originalInput = seed.originalInput
	generatedItems := seed.generatedItems
	rawResponses := seed.rawResponses
	pendingResponse := seed.pendingResponse
	startTurn := seed.startTurn
	shouldRunStartHooks := true

	// cursor tracks what a server-managed conversation already holds
	// (previous_response_id chaining, or a server-side conversation id), so
	// each turn sends only the delta. A resume restores the pause-time cursor
	// from the RunState.
	cursor := seed.cursor

	// pending holds a snapshot supplied by PrepareNextTurn at the last save
	// point, to be used instead of resolving the next turn from the agent.
	var pending *TurnSnapshot

	// finalTurn marks the extra tool-free turn granted when the budget ran out,
	// so it is granted once rather than every turn from then on.
	var finalTurn bool

	// overflowRetries counts "compact and try this turn again" attempts across
	// the whole run, not per turn: a run that overflows on every turn is not
	// recovering, it is looping.
	var overflowRetries int

	// The one-time save of the run's new user input is deferred to just before
	// the first model call (see the persistUserInput call below), so a failure
	// ahead of that — a blocking input-guardrail tripwire, a bad model config —
	// leaves no orphan user message behind. A resume's input was saved before
	// it paused.

	r.log.Info(ctx, "run started",
		slog.Int("max_turns", r.maxTurns),
		slog.Int("tools", len(currentAgent.Tools)),
		slog.Bool("session", r.opts.Conversation.Session != nil))

	// Announce the starting agent before the first turn, for both fresh and
	// resumed runs.
	r.ctrl.setCurrent(currentAgent, startTurn)
	if !r.emit(&AgentUpdatedStreamEvent{NewAgent: currentAgent}) {
		return nil, errConsumerStopped
	}

	for turn := startTurn; ; turn++ {
		// After a completed turn, a caller may ask a streamed run to stop
		// gracefully: the current turn (incl. tools + session save) has finished,
		// so return cleanly with no error before starting the next one.
		if turn > startTurn && r.ctrl.stopRequested() {
			return &RunResult{
				Input:            originalInput,
				NewItems:         r.sessionItems,
				RawResponses:     rawResponses,
				LastAgent:        currentAgent,
				Usage:            r.usageSnapshot(),
				GuardrailResults: r.snapshotGuardrailResults(),
				Diagnostics:      r.diagnostics.All(),
				StoppedEarly:     true,
			}, nil
		}
		if r.maxTurns > 0 && turn > r.maxTurns {
			// One last call with no tools lets the model close out in prose
			// instead of the run ending on an error. It spends a call the
			// budget said not to spend, so it is opt-in — and it is granted by
			// running THIS turn tool-free, not by skipping to the next one.
			// Once granted, the next overrun is the real end.
			if r.opts.Exec.ToolLoop.FinalTurnWithoutTools && !finalTurn {
				r.log.Info(ctx, "turn budget exhausted; one final turn without tools",
					slog.Int("max_turns", r.maxTurns))
				finalTurn = true
			} else {
				r.log.Warn(ctx, "turn budget exhausted", slog.Int("max_turns", r.maxTurns))
				maxErr := newMaxTurnsError(r.maxTurns)
				res, rerr := r.recoverMaxTurns(ctx, maxErr, originalInput, rawResponses, currentAgent)
				if rerr != nil {
					return nil, r.fail(rerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				if res != nil {
					return res, nil
				}
				return nil, r.fail(maxErr, originalInput, generatedItems, rawResponses, currentAgent)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Publish the live turn/agent for RunControl (guarded by a mutex; the
		// run loop and the caller race).
		r.ctrl.setCurrent(currentAgent, turn)
		r.log.Debug(ctx, "turn started", slog.Int("turn", turn), slog.String("agent", currentAgent.Name))

		if shouldRunStartHooks {
			if currentAgent.OnStart != nil {
				if err := currentAgent.OnStart(ctx, r.rc); err != nil {
					return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
			shouldRunStartHooks = false
			// Start an agent span (parent of this agent's generation/tool spans).
			if r.agentSpan != nil {
				r.agentSpan.Finish()
			}
			r.agentSpan = r.trace.StartAgentSpan(currentAgent.Name, r.opts.parentSpanID)
		}

		// Build the model input. In a server-managed mode, only the items the
		// server does not yet have are sent; otherwise the full history is.
		// It can run twice within the first turn: a Blocking input guardrail
		// that REPLACES the input rebuilds from the replacement (see the gate
		// below), so the guarded call itself sees the rewritten input rather
		// than just later turns.
		turnInput, prevID, usedOriginalInput, inputErr := r.buildTurnInput(cursor, originalInput, generatedItems)
		if inputErr != nil {
			return nil, r.fail(inputErr, originalInput, generatedItems, rawResponses, currentAgent)
		}

		snapshot, err := r.buildSnapshot(ctx, currentAgent, turnInput)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		// A turn hook may have replaced the snapshot at the previous save
		// point; from here on the turn reads it and not the agent. Its Input is
		// overwritten: a prepared snapshot is almost always a copy of the
		// previous turn's, and honoring its Input would replay that turn — the
		// tool call and its output silently gone from what the model is sent.
		if pending != nil {
			pending.Input = turnInput
			snapshot = pending
			pending = nil
		}
		model, systemPrompt, prompt := snapshot.Model, snapshot.Instructions, snapshot.Prompt
		outputSchema, handoffs, tools := snapshot.OutputSchema, snapshot.Handoffs, snapshot.Tools
		modelInput := snapshot.Input
		if finalTurn {
			// The point of the extra turn is that the model CANNOT call
			// anything: offered a tool it would call one, and the budget would
			// be exhausted again with nothing said.
			tools, handoffs = nil, nil
		}

		// Publish the turn's input so input guardrails, hooks and tools all see
		// exactly what the model is being sent. CallModelInputFilter may still
		// edit it below, in which case this is refreshed.
		r.rc.setTurnInput(modelInput)

		// On the first turn, run input guardrails. Blocking ones gate the
		// model call and may rewrite this turn's input (Replace); the rest are
		// spawned to race it, delivered through inputRace. They already ran
		// before an interruption, so a resumed run skips them.
		if turn == startTurn && r.resume == nil && !r.inputGuardrailsRan {
			r.inputGuardrailsRan = true
			gate, gerr := r.firstTurnInputGuardrails(ctx, startAgent, originalInput, usedOriginalInput, snapshot,
				func(replaced []TResponseInputItem) ([]TResponseInputItem, error) {
					in, _, _, err := r.buildTurnInput(cursor, replaced, generatedItems)
					return in, err
				})
			originalInput = gate.original
			inputRace = gate.race
			if gerr != nil {
				return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if gate.model != nil {
				modelInput = gate.model
			}
		}

		var resp *ModelResponse
		resumedTurn := pendingResponse != nil
		if resumedTurn {
			// Resuming: re-process the interrupted response (already counted in
			// usage and rawResponses) instead of calling the model again.
			resp = pendingResponse
			pendingResponse = nil
		} else {
			// The one-time user-input save. It lands here, not at loop start, so
			// a failure ahead of the first model call leaves no orphan user
			// message. What that covers depends on the guardrail:
			//
			//   - A Blocking input guardrail has already finished. A tripwire
			//     means nothing is persisted and the model is never called.
			//   - A racing one has not. Its tripwire arrives while the model
			//     call is in flight, so the input IS persisted and the model IS
			//     called (then cancelled) — the documented trade for not
			//     serializing every guardrail ahead of every model call.
			//
			// persistUserInput is idempotent via userInputSaved.
			if r.resume == nil {
				if err := r.persistUserInput(ctx); err != nil {
					return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
			if r.opts.Model.InputFilter != nil {
				edited, ferr := r.opts.Model.InputFilter(ctx, r.rc, currentAgent, ModelInputData{Instructions: systemPrompt, Input: modelInput})
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				systemPrompt, modelInput = edited.Instructions, edited.Input
				// The snapshot IS "what the model is sent this turn"
				// (TurnSnapshot's contract); the turn hooks read it, and
				// PrepareNextTurn may copy it forward. Leaving the pre-filter
				// content there hands them — and the next turn — instructions
				// and input the model never saw.
				snapshot.Instructions, snapshot.Input = systemPrompt, modelInput
				r.rc.setTurnInput(modelInput)
			}
			req := ModelRequest{
				SystemInstructions: systemPrompt,
				Prompt:             prompt,
				Input:              modelInput,
				Settings:           snapshot.Settings,
				Tools:              tools,
				OutputSchema:       outputSchema,
				Handoffs:           handoffs,
				Tracing:            ModelTracingDisabled,
				PreviousResponseID: prevID,
				ConversationID:     r.opts.Conversation.ConversationID,
			}
			r.ctrl.setPhase(PhaseModelCall)
			r.log.Debug(ctx, "calling model",
				slog.Int("turn", turn),
				slog.Int("input_items", len(modelInput)),
				slog.Int("tools", len(tools)),
				Sensitive("instructions", systemPrompt))
			span := r.startGenerationSpan(currentAgent, req)
			// Retries happen inside the model call, where the runner cannot
			// reach; the span rides on the context so they nest under it.
			callCtx := tracing.WithSpan(ctx, span)
			switch {
			case inputRace != nil:
				// First-turn racing input guardrails: the call runs under a
				// cancelable context with the verdict watched from the side.
				out := r.raceModelCall(callCtx, span, model, req, inputRace, originalInput)
				// The race is decided — release its cancel context now, not at
				// loop exit: with a long-lived parent ctx the registration
				// would otherwise outlive the run. Idempotent with the
				// consumer-stopped path's own stop.
				inputRace.stop()
				inputRace = nil
				originalInput = out.original
				if out.stopped {
					return nil, errConsumerStopped
				}
				if out.guardErr != nil {
					return nil, r.fail(out.guardErr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				resp, err = out.resp, out.modelErr
			case r.rawEvents:
				resp, err = r.streamOneModelCall(callCtx, span, model, req)
			default:
				resp, err = model.GetResponse(callCtx, req)
			}
			if err != nil {
				span.SetError(err.Error(), nil)
				span.Finish()
				// The context did not fit. Compaction predicts; this reacts,
				// because the prediction is an estimate against a window the
				// provider never states exactly.
				if r.opts.Exec.Overflow.isOverflow(err, resp) && overflowRetries < r.opts.Exec.Overflow.MaxRetries {
					if compacted, ok := r.recoverOverflow(ctx, err); ok {
						overflowRetries++
						originalInput = compacted
						generatedItems = nil
						// Retry THIS turn: the budget counts model calls the
						// model made, and an overflow is one it never got.
						turn--
						continue
					}
				}
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			r.finishGenerationSpan(span, resp)
			// The model call completed and any first-turn input guardrails passed,
			// so bill usage and surface the response to OnLLMEnd.
			r.rc.Usage.Add(resp.Usage)
			rawResponses = append(rawResponses, resp)
		}
		r.log.Debug(ctx, "model responded",
			slog.Int("turn", turn),
			slog.String("response_id", resp.ResponseID),
			slog.Int("output_items", len(resp.Output)),
			slog.String("status", resp.Status),
			slog.Int64("input_tokens", usageOr(resp.Usage).InputTokens),
			slog.Int64("output_tokens", usageOr(resp.Usage).OutputTokens))
		r.lastResponseID = resp.ResponseID
		r.lastStore = r.resolveSettings(currentAgent).Store
		if !resumedTurn {
			r.lastUsage = resp.Usage
			r.usagePending = true
		} else if r.resume != nil && r.resume.usagePending {
			// The pausing segment stopped with this response's usage still
			// unattributed — the pause-time persist withholds the turn's items
			// (a stored call without its output is a dangling call), so the
			// usage had nothing to land on. The debt transfers to the resumed
			// batch, once. Re-arming unconditionally is what double-counted the
			// request whenever the pausing segment HAD already attributed it.
			r.lastUsage = resp.Usage
			r.usagePending = true
		}

		processed, err := processModelResponse(currentAgent, tools, handoffs, resp, r.opts.Exec.ToolNotFoundBehavior)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		// Emit the model-produced items. A resumed turn re-processes the
		// interrupted response whose items the paused segment already emitted,
		// so only a fresh model call emits here.
		if !resumedTurn {
			for _, it := range processed.NewItems {
				if !r.emitItem(it) {
					return nil, errConsumerStopped
				}
			}
		}

		lenBeforeStep := len(generatedItems)
		step, err := r.executeToolsAndSideEffects(ctx, currentAgent, processed, outputSchema, resumedTurn, originalInput, generatedItems, resp)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		// Emit items produced by side effects (tool/handoff outputs). On a fresh
		// turn NewStepItems begins with processed.NewItems (already emitted
		// above); on a resumed turn it holds only side-effect items, so
		// everything there is new to the stream.
		emitFrom := len(processed.NewItems)
		if resumedTurn {
			emitFrom = 0
		}
		for _, it := range step.NewStepItems[emitFrom:] {
			if !r.emitItem(it) {
				return nil, errConsumerStopped
			}
		}

		generatedItems = append(generatedItems, step.NewStepItems...)
		r.sessionItems = append(r.sessionItems, step.NewStepItems...)
		if len(processed.ToolsUsed) > 0 {
			r.markToolsUsed(currentAgent)
		}

		// Advance the server cursor past this turn: the server now has
		// everything sent plus the model's own output items; synthesized items
		// (tool outputs) remain pending for the next call. A resumed turn
		// re-processes a response the restored cursor already accounts for, so
		// it must not advance — the pre-pause tool outputs are still pending.
		if !resumedTurn {
			cursor.advance(r.opts.Conversation, resp, lenBeforeStep, len(processed.NewItems))
		}

		switch step.NextStep {
		case stepFinalOutput:
			// A steer that arrived too late for the save point, or a queued
			// follow-up, continues the run instead of ending it. The exchange
			// finished on its own terms; the next one starts from it, in the
			// same run, so the trace, the usage total and the session stay one
			// thing rather than three loosely related ones.
			if extra := r.ctrl.takeContinuation(); len(extra) > 0 {
				if err := r.persistSessionItems(ctx); err != nil {
					return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
				}
				injected := injectedInput(currentAgent, extra)
				generatedItems = append(generatedItems, injected...)
				r.sessionItems = append(r.sessionItems, injected...)
				r.injectedUpTo = len(r.sessionItems)
				for _, it := range injected {
					if !r.emitItem(it) {
						return nil, errConsumerStopped
					}
				}
				continue
			}
			res, ferr := r.finishRun(ctx, currentAgent, originalInput, rawResponses, step.FinalOutput)
			if ferr != nil {
				return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
			}
			return res, nil
		case stepHandoff:
			// Persist this turn before switching agents. sessionItems is the
			// unfiltered log, so the handoff input filter below (which rewrites
			// generatedItems) never affects what the session keeps.
			if err := r.persistSessionItems(ctx); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			// A handoff is a turn boundary too: control is about to leave this
			// agent, which is exactly the moment a caller may want to stop.
			// Asked before the input filter runs, so the hook sees the turn as
			// it happened. The rest of the save point does not apply — the next
			// turn belongs to a different agent, so its snapshot is resolved
			// fresh and its context is about to be rewritten by the filter.
			stop, out, serr := r.stopAfterTurn(ctx, currentAgent, turn, resp, snapshot, step.NewStepItems)
			if serr != nil {
				return nil, r.fail(serr, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if stop {
				res, ferr := r.finishRun(ctx, currentAgent, originalInput, rawResponses, out)
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				return res, nil
			}
			if step.Handoff != nil {
				if filter := r.handoffInputFilter(step.Handoff); filter != nil {
					// A handoff input filter cannot coexist with server-managed
					// conversation state: the server holds the unfiltered history,
					// so a filtered view would desync (in ConversationID mode,
					// resending the full filtered input duplicates the server's
					// stored items). Fail fast, matching Python's UserError.
					if r.opts.Conversation.UsePreviousResponseID || r.opts.Conversation.ConversationID != "" {
						err := newUserError("handoff input filters (including NestHandoffHistory) are not supported with server-managed conversation state (UsePreviousResponseID / ConversationID)")
						return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
					}
					filtered, ferr := applyHandoffInputFilter(filter, originalInput, generatedItems)
					if ferr != nil {
						return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
					}
					originalInput = filtered
					generatedItems = nil
				}
			}
			r.log.Info(ctx, "handoff",
				slog.String("from", currentAgent.Name), slog.String("to", step.NewAgent.Name))
			currentAgent = step.NewAgent
			shouldRunStartHooks = true
			if !r.emit(&AgentUpdatedStreamEvent{NewAgent: currentAgent}) {
				return nil, errConsumerStopped
			}
			continue
		case stepInterruption:
			// Injections taken this turn were consumed by the interrupted
			// turn's model call and ride in the state's item log
			// (SessionItems), which resume persists — that log, not
			// PendingInput, is their durable home now. Committing here keeps a
			// resume from delivering them a second time via restore.
			r.ctrl.commitInjected()
			// Persist the completed part of this turn before pausing. The pending
			// tool calls have no outputs yet, so persistSessionItems holds them
			// back (they would break replay); they save with their outputs once
			// the run resumes. The cursor rides along in RunState.
			if err := r.persistSessionItems(ctx); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			// Snapshot any nested states already cached on the run context under the
			// mutex that guards them (run_context.go's nestedMu contract): a
			// timed-out tool can leave an orphan goroutine that still calls
			// takeNestedToolState concurrently with this read.
			r.rc.nestedMu.Lock()
			carriedNested := maps.Clone(r.rc.nestedToolStates)
			r.rc.nestedMu.Unlock()
			state := &RunState{
				CurrentAgent:          currentAgent,
				OriginalInput:         originalInput,
				GeneratedItems:        generatedItems,
				SessionItems:          r.sessionItems,
				PersistedSessionItems: r.persistedSessionItems,
				UserInput:             r.userInput,
				RawResponses:          rawResponses,
				InterruptedResponse:   resp,
				Interruptions:         step.Interruptions,
				Approvals:             r.rc.Approvals,
				Usage:                 r.usageSnapshot(),
				CurrentTurn:           turn,
				MaxTurns:              r.maxTurns,
				ToolsUsed:             toolsUsedList(r.toolsUsedBy),
				PendingInput:          r.ctrl.Pending(),
				DisclosedTools:        sortedKeys(r.disclosed),
				ReasoningItemIDPolicy: r.opts.Exec.ReasoningItemIDPolicy,
				cursor:                cursor,
				// Carry the guardrail results accumulated so far so a resumed run's
				// RunResult still reports them: first-turn input guardrails are not
				// re-run on resume (Python parity), so this is their only source.
				GuardrailResults: r.snapshotGuardrailResults(),
				// Carry any paused agent-as-tool nested states so ResumeRun
				// continues them; merge with any already cached on the run context
				// from an earlier resume of the same parent run. Serialized in
				// RunState JSON, so a cross-process resume continues them too.
				nestedToolStates: mergeNestedStates(carriedNested, step.NestedStates),
				// Whether this response's usage still needs attributing; the
				// resumed batch settles the debt exactly once (see attributeUsage).
				usagePending: r.usagePending,
			}
			return &RunResult{
				Input: originalInput,
				// Unfiltered log for observability (State.GeneratedItems keeps the
				// filtered view for resume correctness).
				NewItems:         r.sessionItems,
				RawResponses:     rawResponses,
				LastAgent:        currentAgent,
				Usage:            r.usageSnapshot(),
				GuardrailResults: r.snapshotGuardrailResults(),
				Diagnostics:      r.diagnostics.All(),
				Interruptions:    step.Interruptions,
				State:            state,
			}, nil
		case stepRunAgain:
			sp, serr := r.savePoint(ctx, savePointInput{
				Turn:     turn,
				Agent:    currentAgent,
				Snapshot: snapshot,
				Response: resp,
				NewItems: step.NewStepItems,
			})
			if serr != nil {
				return nil, r.fail(serr, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if sp.Stop {
				res, ferr := r.finishRun(ctx, currentAgent, originalInput, rawResponses, sp.FinalOutput)
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				return res, nil
			}
			if sp.Recompacted {
				// The rebuilt context already contains this run's items, so the
				// generated list starts over — the same substitution a handoff
				// input filter makes above.
				originalInput = sp.Input
				generatedItems = nil
			}
			if len(sp.Injected) > 0 {
				generatedItems = append(generatedItems, sp.Injected...)
				r.sessionItems = append(r.sessionItems, sp.Injected...)
				r.injectedUpTo = len(r.sessionItems)
				for _, it := range sp.Injected {
					if !r.emitItem(it) {
						return nil, errConsumerStopped
					}
				}
			}
			pending = sp.NextSnapshot
			continue
		}
	}
}
