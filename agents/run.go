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
// input may be a string or a []InputItem; use InputItemsFromText for
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
			yield(nil, NewUserError("run stream already consumed: a RunStream is single-use, and ranging it again would re-execute the run"))
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
// the outcome as the stream's terminal event or terminal error. Its input is
// already normalized — runViaMiddleware does that once, up front, so that a
// middleware and the loop see the same list.
func runStream(ctx context.Context, agent *Agent, input []InputItem, opts RunOptions, ctrl *runControl, rawEvents bool, yield func(StreamEvent, error) bool) {
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

// turnState is what the loop carries from turn to turn: the run's input as the
// model sees it, everything generated so far, every response received, and the
// agent currently holding the turn.
//
// The four live as one value because every way a run reports itself — a
// result, a failure, a pause state — reads all of them. They used to travel as
// four parameters through twenty-odd call sites, and being two slices of
// pointers, a slice and a pointer, a transposed pair compiled clean.
//
// Two stages replace fields wholesale rather than appending: a handoff input
// filter and a recompaction (mid-run or after an overflow) rewrite
// originalInput and clear generatedItems, so the model's view restarts from
// the rewritten context. runner.sessionItems is deliberately NOT part of this
// — it is the unfiltered log those resets must not touch.
type turnState struct {
	originalInput  []InputItem
	generatedItems []*RunItem
	rawResponses   []*ModelResponse
	agent          *Agent
}

// runner holds the mutable state for a single Run invocation.
type runner struct {
	opts      RunOptions
	rc        *RunContext
	maxTurns  int
	userInput []InputItem          // the new input to persist to the session
	resume    *RunState            // non-nil when resuming from an interruption
	trace     *tracing.TraceHandle // non-nil when tracing is enabled
	agentSpan *tracing.SpanHandle  // current agent span, parent of generation/tool spans

	// state is the loop's carried turn state (see turnState). It is on the
	// runner rather than in locals so fail, baseResult, finishRun and
	// buildPauseState report the run without being handed the same four values
	// at every call site. loop sets it before the first turn; nothing outside
	// the loop and the stages it calls reads it.
	state *turnState

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
	// Respond call instead.
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
	sessionItems []*RunItem

	// persistedSessionItems counts how many leading sessionItems have already
	// been written to the session. The loop persists incrementally — after each
	// turn and at an interruption — so a cancelled or failed run keeps every
	// completed turn instead of losing the whole run. Carried across
	// interrupt/resume in RunState.
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

	// inputRace holds the first turn's non-blocking input guardrails while they
	// race the model call, so callModelOnce can hand that call to raceModelCall
	// and the loop's deferred stop cancels them however the run exits. Cleared
	// once the race is decided.
	inputRace *inputGuardRace

	// overflowRetries counts "compact and try this turn again" attempts across
	// the whole run, not per turn: a run that overflows on every turn is not
	// recovering, it is looping.
	overflowRetries int

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
	// tool_choice reset (Agent.DisableToolChoiceReset). Keyed by agent name so it
	// can be carried across an interrupt/resume in RunState, keeping the reset in
	// effect for every agent that used tools before the pause — not just the
	// interrupted one.
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

func (r *runner) loop(ctx context.Context, startAgent *Agent, originalInput []InputItem) (*RunResult, error) {
	// Finish the active agent span when the loop ends (nil-safe when untraced).
	defer func() { r.agentSpan.Finish() }()

	// r.inputRace holds the first turn's racing input guardrails once spawned.
	// The deferred stop covers every early exit (e.g. a failed model call), so
	// an LLM-based guardrail is cancelled instead of running to completion
	// after the run has already returned.
	defer func() { r.inputRace.stop() }()

	// When resuming from an interruption, the seed carries the prior state and
	// the captured response, re-processed on the first iteration instead of
	// calling the model.
	seed := r.seedLoop(startAgent, originalInput)
	st := &turnState{
		originalInput:  seed.originalInput,
		generatedItems: seed.generatedItems,
		rawResponses:   seed.rawResponses,
		agent:          seed.agent,
	}
	r.state = st
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

	// Nothing is persisted here: the one-time save of the run's new user input
	// is deferred to just before the first model call (callModelOnce), so a
	// failure ahead of that — a blocking input-guardrail tripwire, a bad model
	// config — leaves no orphan user message behind.

	r.log.Info(ctx, "run started",
		slog.Int("max_turns", r.maxTurns),
		slog.Int("tools", len(st.agent.Tools)),
		slog.Bool("session", r.opts.Conversation.Session != nil))

	// Announce the starting agent before the first turn, for both fresh and
	// resumed runs.
	if !r.emit(&AgentUpdatedStreamEvent{NewAgent: st.agent}) {
		return nil, errConsumerStopped
	}

	for turn := startTurn; ; turn++ {
		// After a completed turn, a caller may ask a streamed run to stop
		// gracefully: the current turn (incl. tools + session save) has finished,
		// so return cleanly with no error before starting the next one.
		if turn > startTurn && r.ctrl.stopRequested() {
			res := r.baseResult()
			res.StoppedEarly = true
			return res, nil
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
				maxErr := &MaxTurnsError{MaxTurns: r.maxTurns}
				res, rerr := r.recoverMaxTurns(ctx, maxErr)
				if rerr != nil {
					return nil, r.fail(rerr)
				}
				if res != nil {
					return res, nil
				}
				return nil, r.fail(maxErr)
			}
		}
		// A cancellation caught at the turn boundary fails the run like any
		// other mid-loop error: the caller reaches the completed turns through
		// RunError.Result rather than getting a bare context error with the
		// progress dropped (errors.go: only failures from BEFORE the loop are
		// returned bare).
		if err := ctx.Err(); err != nil {
			return nil, r.fail(err)
		}
		r.log.Debug(ctx, "turn started", slog.Int("turn", turn), slog.String("agent", st.agent.Name))

		if shouldRunStartHooks {
			if st.agent.OnStart != nil {
				if err := st.agent.OnStart(ctx, r.rc); err != nil {
					return nil, r.fail(err)
				}
			}
			shouldRunStartHooks = false
			// Start an agent span (parent of this agent's generation/tool spans).
			if r.agentSpan != nil {
				r.agentSpan.Finish()
			}
			r.agentSpan = r.trace.StartAgentSpan(st.agent.Name, r.opts.parentSpanID)
		}

		// Build the model input. In a server-managed mode, only the items the
		// server does not yet have are sent; otherwise the full history is.
		// It can run twice within the first turn: a Blocking input guardrail
		// that REPLACES the input rebuilds from the replacement (see the gate
		// below), so the guarded call itself sees the rewritten input rather
		// than just later turns.
		turnInput, prevID, usedOriginalInput, inputErr := r.buildTurnInput(cursor, st.originalInput, st.generatedItems)
		if inputErr != nil {
			return nil, r.fail(inputErr)
		}

		snapshot, err := r.buildSnapshot(ctx, st.agent, turnInput)
		if err != nil {
			return nil, r.fail(err)
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
			gate, gerr := r.firstTurnInputGuardrails(ctx, startAgent, st.originalInput, usedOriginalInput, snapshot,
				func(replaced []InputItem) ([]InputItem, error) {
					in, _, _, err := r.buildTurnInput(cursor, replaced, st.generatedItems)
					return in, err
				})
			st.originalInput = gate.original
			r.inputRace = gate.race
			if gerr != nil {
				return nil, r.fail(gerr)
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
			out := r.callModelOnce(ctx, turn, snapshot, ModelRequest{
				SystemInstructions: snapshot.Instructions,
				Prompt:             snapshot.Prompt,
				Input:              modelInput,
				Settings:           snapshot.Settings,
				Tools:              tools,
				OutputSchema:       outputSchema,
				Handoffs:           handoffs,
				PreviousResponseID: prevID,
				ConversationID:     r.opts.Conversation.ConversationID,
			})
			if out.err != nil {
				return nil, out.err
			}
			if out.retry {
				// The context overflowed and compaction produced a shorter one.
				// Retry THIS turn: the budget counts model calls the model made,
				// and an overflow is one it never got.
				turn--
				continue
			}
			resp = out.resp
		}
		r.log.Debug(ctx, "model responded",
			slog.Int("turn", turn),
			slog.String("response_id", resp.ResponseID),
			slog.Int("output_items", len(resp.Output)),
			slog.String("status", resp.Status),
			slog.Int64("input_tokens", usageOr(resp.Usage).InputTokens),
			slog.Int64("output_tokens", usageOr(resp.Usage).OutputTokens))
		r.lastResponseID = resp.ResponseID
		// Read the settings the REQUEST carried, not the agent's: a turn hook
		// can replace the whole snapshot, and re-resolving would report a
		// Store the call never used — compaction would then treat an unstored
		// response id as one the server still holds.
		if snapshot.Settings != nil {
			r.lastStore = snapshot.Settings.Store
		} else {
			r.lastStore = nil
		}
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

		processed, err := processModelResponse(st.agent, tools, handoffs, resp, r.opts.Exec.ToolNotFoundBehavior)
		if err != nil {
			return nil, r.fail(err)
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

		lenBeforeStep := len(st.generatedItems)
		step, err := r.executeToolsAndSideEffects(ctx, st.agent, processed, outputSchema, resumedTurn, stepProgress{
			originalInput: st.originalInput,
			preStepItems:  st.generatedItems,
			resp:          resp,
		})
		if err != nil {
			return nil, r.fail(err)
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

		st.generatedItems = append(st.generatedItems, step.NewStepItems...)
		r.sessionItems = append(r.sessionItems, step.NewStepItems...)
		if len(processed.ToolsUsed) > 0 {
			r.markToolsUsed(st.agent)
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
				// Append the take BEFORE persisting, so the write that closes
				// this exchange also covers the injected input — the boundary
				// check in persistSessionItems then commits the take only once
				// a write has genuinely persisted past it. Persisting first
				// committed the take against a write that predated it, and a
				// failure later in the run could no longer roll it back.
				injected := injectedInput(st.agent, extra)
				st.generatedItems = append(st.generatedItems, injected...)
				r.sessionItems = append(r.sessionItems, injected...)
				r.injectedUpTo = len(r.sessionItems)
				if err := r.persistSessionItems(ctx); err != nil {
					return nil, r.fail(err)
				}
				for _, it := range injected {
					if !r.emitItem(it) {
						return nil, errConsumerStopped
					}
				}
				continue
			}
			res, ferr := r.finishRun(ctx, step.FinalOutput)
			if ferr != nil {
				return nil, r.fail(ferr)
			}
			return res, nil
		case stepHandoff:
			// Persist this turn before switching agents. sessionItems is the
			// unfiltered log, so the handoff input filter below (which rewrites
			// generatedItems) never affects what the session keeps.
			if err := r.persistSessionItems(ctx); err != nil {
				return nil, r.fail(err)
			}
			// A handoff is a turn boundary too: control is about to leave this
			// agent, which is exactly the moment a caller may want to stop.
			// Asked before the input filter runs, so the hook sees the turn as
			// it happened. The rest of the save point does not apply — the next
			// turn belongs to a different agent, so its snapshot is resolved
			// fresh and its context is about to be rewritten by the filter.
			stop, out, serr := r.stopAfterTurn(ctx, st.agent, &TurnResult{
				Turn: turn, Response: resp, NewItems: step.NewStepItems, Snapshot: snapshot,
			})
			if serr != nil {
				return nil, r.fail(serr)
			}
			if stop {
				res, ferr := r.finishRun(ctx, out)
				if ferr != nil {
					return nil, r.fail(ferr)
				}
				return res, nil
			}
			if step.Handoff != nil {
				if filter := r.handoffInputFilter(step.Handoff); filter != nil {
					// A handoff input filter cannot coexist with server-managed
					// conversation state: the server holds the unfiltered history,
					// so a filtered view would desync (in ConversationID mode,
					// resending the full filtered input duplicates the server's
					// stored items). Fail fast.
					if r.opts.Conversation.UsePreviousResponseID || r.opts.Conversation.ConversationID != "" {
						err := NewUserError("handoff input filters (including NestHandoffHistory) are not supported with server-managed conversation state (UsePreviousResponseID / ConversationID)")
						return nil, r.fail(err)
					}
					filtered, ferr := applyHandoffInputFilter(filter, st.originalInput, st.generatedItems)
					if ferr != nil {
						return nil, r.fail(ferr)
					}
					st.originalInput = filtered
					st.generatedItems = nil
				}
			}
			r.log.Info(ctx, "handoff",
				slog.String("from", st.agent.Name), slog.String("to", step.NewAgent.Name))
			st.agent = step.NewAgent
			shouldRunStartHooks = true
			if !r.emit(&AgentUpdatedStreamEvent{NewAgent: st.agent}) {
				return nil, errConsumerStopped
			}
			continue
		case stepInterruption:
			// Persist the completed part of this turn before pausing. The pending
			// tool calls have no outputs yet, so persistSessionItems holds them
			// back (they would break replay); they save with their outputs once
			// the run resumes. The cursor rides along in RunState.
			if err := r.persistSessionItems(ctx); err != nil {
				return nil, r.fail(err)
			}
			// Injections taken this turn were consumed by the interrupted
			// turn's model call and ride in the state's item log
			// (SessionItems), which resume persists — that log, not
			// PendingInput, is their durable home now. Committed only AFTER
			// the persist above succeeds: a failed persist fails the attempt
			// before any RunState exists, and the rollback in finishStream
			// must still find the take to redeliver it on a retry.
			r.ctrl.commitInjected()
			res := r.baseResult()
			res.Interruptions = step.Interruptions
			res.State = r.buildPauseState(turn, resp, step, cursor)
			return res, nil
		case stepRunAgain:
			sp, serr := r.savePoint(ctx, savePointInput{
				Turn:     turn,
				Agent:    st.agent,
				Snapshot: snapshot,
				Response: resp,
				NewItems: step.NewStepItems,
			})
			if serr != nil {
				return nil, r.fail(serr)
			}
			if sp.Stop {
				res, ferr := r.finishRun(ctx, sp.FinalOutput)
				if ferr != nil {
					return nil, r.fail(ferr)
				}
				return res, nil
			}
			if sp.Recompacted {
				// The rebuilt context already contains this run's items, so the
				// generated list starts over — the same substitution a handoff
				// input filter makes above.
				st.originalInput = sp.Input
				st.generatedItems = nil
			}
			if len(sp.Injected) > 0 {
				st.generatedItems = append(st.generatedItems, sp.Injected...)
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

// modelCallOutcome is how one turn's model call ended. Exactly one of the three
// is meaningful: resp when the model answered, retry when the context
// overflowed and compaction produced a shorter one worth trying the same turn
// against, err otherwise.
//
// err is already in the shape the loop returns — fail-wrapped, or the bare
// errConsumerStopped — so the caller passes it straight out.
type modelCallOutcome struct {
	resp  *ModelResponse
	retry bool
	err   error
}

// callModelOnce performs one turn's model call and everything that wraps it:
// the one-time user-input save, ModelOptions.InputFilter, the generation span,
// the three shapes the call can take (raced by the first turn's non-blocking
// input guardrails, streamed, or plain), and the overflow recovery that asks
// for the turn to be run again.
//
// req is the request as the loop resolved it. InputFilter may still rewrite its
// instructions and input, in which case snap and the run context are updated to
// match: a snapshot IS "what the model is sent this turn" (TurnSnapshot's
// contract), it is what the turn hooks read, and PrepareNextTurn may copy it
// forward — leaving the pre-filter content there would hand them, and the next
// turn, instructions and input the model never saw.
func (r *runner) callModelOnce(ctx context.Context, turn int, snap *TurnSnapshot, req ModelRequest) modelCallOutcome {
	st := r.state
	// The one-time user-input save. It lands here, not at loop start, so a
	// failure ahead of the first model call leaves no orphan user message. What
	// that covers depends on the guardrail:
	//
	//   - A Blocking input guardrail has already finished. A tripwire means
	//     nothing is persisted and the model is never called.
	//   - A racing one has not. Its tripwire arrives while the model call is in
	//     flight, so the input IS persisted and the model IS called (then
	//     cancelled) — the documented trade for not serializing every guardrail
	//     ahead of every model call.
	//
	// A resumed run skips it: its input was saved before it paused.
	// persistUserInput is itself idempotent via userInputSaved.
	if r.resume == nil {
		if err := r.persistUserInput(ctx); err != nil {
			return modelCallOutcome{err: r.fail(err)}
		}
	}
	if r.opts.Model.InputFilter != nil {
		edited, ferr := r.opts.Model.InputFilter(ctx, r.rc, st.agent,
			ModelInputData{Instructions: req.SystemInstructions, Input: req.Input})
		if ferr != nil {
			return modelCallOutcome{err: r.fail(ferr)}
		}
		req.SystemInstructions, req.Input = edited.Instructions, edited.Input
		snap.Instructions, snap.Input = edited.Instructions, edited.Input
		r.rc.setTurnInput(req.Input)
	}
	r.log.Debug(ctx, "calling model",
		slog.Int("turn", turn),
		slog.Int("input_items", len(req.Input)),
		slog.Int("tools", len(req.Tools)),
		Sensitive("instructions", req.SystemInstructions))
	span := r.startGenerationSpan(st.agent, req)
	// Retries happen inside the model call, where the runner cannot reach; the
	// span rides on the context so they nest under it.
	callCtx := tracing.WithSpan(ctx, span)
	var resp *ModelResponse
	var err error
	switch {
	case r.inputRace != nil:
		// First-turn racing input guardrails: the call runs under a cancelable
		// context with the verdict watched from the side.
		out := r.raceModelCall(callCtx, span, snap.Model, req, r.inputRace, st.originalInput)
		// The race is decided — release its cancel context now, not at loop
		// exit: with a long-lived parent ctx the registration would otherwise
		// outlive the run. Idempotent with the consumer-stopped path's own stop.
		r.inputRace.stop()
		r.inputRace = nil
		st.originalInput = out.original
		if out.stopped {
			return modelCallOutcome{err: errConsumerStopped}
		}
		if out.guardErr != nil {
			return modelCallOutcome{err: r.fail(out.guardErr)}
		}
		resp, err = out.resp, out.modelErr
	case r.rawEvents:
		resp, err = r.streamOneModelCall(callCtx, span, snap.Model, req)
	default:
		resp, err = snap.Model.Respond(callCtx, req)
	}
	if err != nil {
		span.SetError(err.Error(), nil)
		span.Finish()
		// The context did not fit. Compaction predicts; this reacts, because
		// the prediction is an estimate against a window the provider never
		// states exactly.
		if r.opts.Exec.Overflow.isOverflow(err) && r.overflowRetries < r.opts.Exec.Overflow.MaxRetries {
			if compacted, ok := r.recoverOverflow(ctx, err); ok {
				r.overflowRetries++
				st.originalInput = compacted
				st.generatedItems = nil
				return modelCallOutcome{retry: true}
			}
		}
		return modelCallOutcome{err: r.fail(err)}
	}
	r.finishGenerationSpan(span, resp)
	// The model call completed and any first-turn input guardrails passed, so
	// bill usage and surface the response to OnLLMEnd.
	r.rc.Usage.Add(resp.Usage)
	st.rawResponses = append(st.rawResponses, resp)
	return modelCallOutcome{resp: resp}
}

// buildPauseState captures everything ResumeRun needs to continue this run,
// here or in another process: the loop's carried state, the runner's
// persistence, usage and disclosure bookkeeping, the server cursor, and the
// interruption that stopped it.
func (r *runner) buildPauseState(turn int, resp *ModelResponse, step *singleStepResult, cursor serverCursor) *RunState {
	// Snapshot any nested states already cached on the run context under the
	// mutex that guards them (run_context.go's nestedMu contract): a timed-out
	// tool can leave an orphan goroutine that still calls takeNestedToolState
	// concurrently with this read.
	r.rc.nestedMu.Lock()
	carriedNested := maps.Clone(r.rc.nestedToolStates)
	r.rc.nestedMu.Unlock()
	st := r.state
	return &RunState{
		CurrentAgent:          st.agent,
		OriginalInput:         st.originalInput,
		GeneratedItems:        st.generatedItems,
		SessionItems:          r.sessionItems,
		PersistedSessionItems: r.persistedSessionItems,
		UserInput:             r.userInput,
		RawResponses:          st.rawResponses,
		InterruptedResponse:   resp,
		Interruptions:         step.Interruptions,
		Approvals:             r.rc.Approvals,
		Usage:                 r.usageSnapshot(),
		CurrentTurn:           turn,
		MaxTurns:              r.maxTurns,
		ToolsUsed:             sortedKeys(r.toolsUsedBy),
		PendingInput:          r.ctrl.Pending(),
		DisclosedTools:        sortedKeys(r.disclosed),
		ReasoningItemIDPolicy: r.opts.Exec.ReasoningItemIDPolicy,
		cursor:                cursor,
		// Carry the guardrail results accumulated so far so a resumed run's
		// RunResult still reports them: first-turn input guardrails are not
		// re-run on resume, so this is their only source.
		GuardrailResults: r.snapshotGuardrailResults(),
		// Carry any paused agent-as-tool nested states so ResumeRun continues
		// them; merge with any already cached on the run context from an earlier
		// resume of the same parent run. Serialized in RunState JSON, so a
		// cross-process resume continues them too.
		nestedToolStates: mergeNestedStates(carriedNested, step.NestedStates),
		// Whether this response's usage still needs attributing; the resumed
		// batch settles the debt exactly once (see attributeUsage).
		usagePending: r.usagePending,
	}
}
