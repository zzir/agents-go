package agents

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/zzir/agents-go/tracing"
)

// Run starts an agent run and returns it as a stream plus a control handle.
// Nothing executes until the stream is ranged: the run happens on the
// consumer's goroutine, and abandoning the stream stops the run (spec §2.0).
// input is a string or a []InputItem.
//
//	stream, ctrl := agents.Run(ctx, agent, "hi", agents.RunOptions{})
//	for ev, err := range stream {
//	    if err != nil { return err }
//	    ...
//	}
//
// To stop cleanly at a turn boundary, call ctrl.StopAfterTurn and keep
// ranging. A ToolProgressEvent is yielded from the tool's own goroutine
// (spec §2.7g; docs/howto/streaming.md).
func Run(ctx context.Context, agent *Agent, input any, opts RunOptions) (RunStream, RunControl) {
	ctrl := newRunControl()
	return withMiddleware(ctx, agent, input, opts, ctrl, true), ctrl
}

// RunSync executes a run to completion and returns its result: Run without
// the stream, so the model is called without streaming and no raw events are
// produced.
func RunSync(ctx context.Context, agent *Agent, input any, opts RunOptions) (*RunResult, error) {
	ctrl := newRunControl()
	return withMiddleware(ctx, agent, input, opts, ctrl, false).Collect()
}

// singleUse guards a RunStream against being ranged twice: a second range
// would silently re-execute the run, so it yields an error instead.
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

// withMiddleware builds the run's stream through the middleware chain; input
// is normalized once, up front, so a middleware edits the list the loop uses.
func withMiddleware(ctx context.Context, agent *Agent, input any, opts RunOptions, ctrl *runControl, rawEvents bool) RunStream {
	base := func(ctx context.Context, in RunInput) RunStream {
		return func(yield func(StreamEvent, error) bool) {
			runStream(ctx, in.Agent, in.Input, *in.Opts, ctrl, rawEvents, yield)
		}
	}
	return runViaMiddleware(ctx, agent, input, opts, ctrl, base)
}

// runViaMiddleware is the pipeline fresh runs and resumes share: normalize the
// input once, wrap base in the middleware chain, hand out a single-use stream.
func runViaMiddleware(ctx context.Context, agent *Agent, input any, opts RunOptions, ctrl *runControl, base RunFunc) RunStream {
	return singleUse(func(yield func(StreamEvent, error) bool) {
		items, err := normalizeInput(input)
		if err != nil {
			yield(nil, err)
			return
		}
		in := RunInput{Agent: agent, Input: items, Opts: &opts, Control: ctrl}
		for ev, ierr := range chainMiddleware(base, opts.Middlewares)(ctx, in) {
			if !yield(ev, ierr) {
				return
			}
		}
	})
}

// runStream is the body shared by Run and RunSync: prepare, loop, and report
// the outcome as the stream's terminal event or error. Input is normalized.
func runStream(ctx context.Context, agent *Agent, input []InputItem, opts RunOptions, ctrl *runControl, rawEvents bool, yield func(StreamEvent, error) bool) {
	r, modelInput, finishTrace, err := prepareRun(ctx, agent, input, opts)
	if err != nil {
		yield(nil, err)
		return
	}
	defer finishTrace()
	r.yield, r.ctrl, r.rawEvents = yield, ctrl, rawEvents
	res, err := r.execute(ctx, agent, modelInput)
	r.finishStream(res, err)
}

// execute runs the loop under the run's own diagnostics sink and cancellation
// root, which emit cancels (cause errConsumerStopped) when the consumer leaves.
func (r *runner) execute(ctx context.Context, agent *Agent, input []InputItem) (*RunResult, error) {
	ctx = WithDiagnostics(ctx, r.diagnostics)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.cancelRun = cancel
	return r.loop(ctx, agent, input)
}

// finishStream reports a completed loop to the consumer and closes the stream
// behind it. A consumer that already stopped is told nothing further.
func (r *runner) finishStream(res *RunResult, err error) {
	// A completed attempt delivered its injection take; a failed or abandoned
	// one returns it so a retrying middleware's next attempt delivers it.
	if err != nil || r.closed.Load() {
		r.ctrl.rollbackInjected()
	} else {
		r.ctrl.commitInjected()
	}
	// Closed under emitMu, before it is released: a tool goroutine that
	// outlived its call finds the stream closed, not a returned yield.
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	defer r.closed.Store(true)
	if r.closed.Load() || errors.Is(err, errConsumerStopped) {
		return
	}
	if err != nil {
		r.yield(nil, err)
		return
	}
	r.yield(&RunCompletedEvent{Result: res}, nil)
}

// turnState is what the loop carries from turn to turn. The item log itself is
// runner.sessionItems; the model's view of it is the tail from generatedFrom.
type turnState struct {
	originalInput []InputItem
	rawResponses  []*ModelResponse
	agent         *Agent

	// startTurn is the first turn this loop runs (past 1 on a resume);
	// pendingResponse is a resume's interrupted response, re-processed on that
	// turn instead of calling the model.
	startTurn       int
	pendingResponse *ModelResponse

	// cursor is what a server-managed conversation already holds, so each turn
	// sends only the delta. A resume restores the pause-time cursor.
	cursor serverCursor

	// pending is PrepareNextTurn's snapshot for the next turn; runStartHooks
	// gates OnStart and the agent span; finalTurn is the tool-free grace turn.
	pending       *TurnSnapshot
	runStartHooks bool
	finalTurn     bool
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

	// state is the loop's carried turn state (see turnState), on the runner so
	// fail, baseResult, finishRun and buildPauseState can report the run.
	state *turnState

	// yield delivers events to the consumer. Always set (RunSync discards);
	// once it returns false emit records closed and the loop unwinds.
	yield func(StreamEvent, error) bool

	// ctrl is the handle the caller got back from Run: the graceful-stop flag
	// and the injection queue.
	ctrl *runControl

	// rawEvents streams the model call so raw events reach the consumer — the
	// one difference between Run and RunSync.
	rawEvents bool

	// closed marks the stream ended; emit yields nothing after it. Atomic: a
	// tool goroutine may set it while the loop reads it lock-free.
	closed atomic.Bool

	// cancelRun cancels the run's context (cause errConsumerStopped) when the
	// consumer stops ranging — spec §2.0.
	cancelRun context.CancelCauseFunc

	// sessionItems is the run's item log: everything the run produced, in
	// order, for RunResult.NewItems and session persistence. Append-only.
	sessionItems []*RunItem

	// generatedFrom is where the model's view of the log begins (see
	// generatedItems); a handoff filter or recompaction moves it — spec §2.1.
	generatedFrom int

	// persistedSessionItems counts how many leading sessionItems the session
	// already holds; carried across interrupt/resume in RunState.
	persistedSessionItems int

	// userInputSaved guards the one-time persistence of userInput.
	userInputSaved bool

	// emitMu serializes yields: tools push progress from their own goroutines.
	emitMu sync.Mutex

	// diagnostics collects trouble the run survived. Never nil.
	diagnostics *DiagnosticSink
	// diagnosticsSaved is how many diagnostics are already attached to
	// entries, so each lands on the turn it happened in.
	diagnosticsSaved int

	// log is the run's logger, already tagged with the run's identity. Never
	// nil — a disabled logger is a no-op rather than a check at every site.
	log *runLogger

	// lastUsage is the most recent model response's usage, held so the entries
	// that response produced can carry it.
	lastUsage *Usage

	// usagePending marks a model response whose usage has not yet landed on an
	// entry; cleared on attribution — spec §2.7f.
	usagePending bool

	// inputGuardrailsRan keeps an overflow retry of the first turn from running
	// the input guardrails a second time.
	inputGuardrailsRan bool

	// inputRace holds the first turn's non-blocking input guardrails while they
	// race the model call; the loop's deferred stop cancels them on any exit.
	inputRace *inputGuardRace

	// overflowRetries counts compact-and-retry attempts across the whole run,
	// not per turn.
	overflowRetries int

	// injectedUpTo is the sessionItems length just past the latest injected
	// input; a session write commits the in-flight injections only once it has
	// persisted past it.
	injectedUpTo int

	// disclosed names the deferred tools a ToolResult has opened up; carried on
	// RunState so a resume does not re-hide them.
	disclosed map[string]bool

	// consecutiveErrorTurns counts turns in a row where every tool call failed
	// (spec §2.7d).
	consecutiveErrorTurns int

	// toolsUsedBy tracks which agents have called tools (tool_choice reset).
	// Keyed by name so RunState carries it across a pause for every agent.
	toolsUsedBy map[string]bool

	// lastResponseID / lastStore record the final model call's response id and
	// store setting, used to drive session compaction after persistence.
	lastResponseID string
	lastStore      *bool

	// offChainHistory records that the stored log holds items no model call in
	// this run carried. Monotone; carried on RunState — see offChainItems.
	offChainHistory bool

	// guardrailMu guards guardrailResults: tool stages record from per-call
	// goroutines while input/output stages record from the main loop.
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
	// The deferred stop covers every early exit, so a racing LLM-based
	// guardrail is cancelled instead of running on after the run returned.
	defer func() { r.inputRace.stop() }()

	seed := r.seedLoop(startAgent, originalInput)
	st := &turnState{
		originalInput:   seed.originalInput,
		rawResponses:    seed.rawResponses,
		agent:           seed.agent,
		startTurn:       seed.startTurn,
		pendingResponse: seed.pendingResponse,
		cursor:          seed.cursor,
		runStartHooks:   true,
	}
	r.state = st

	r.log.Info(ctx, "run started",
		slog.Int("max_turns", r.maxTurns),
		slog.Int("tools", len(st.agent.Tools)),
		slog.Bool("session", r.opts.Conversation.Session != nil))

	// Announce the starting agent before the first turn, for both fresh and
	// resumed runs.
	if !r.emit(&AgentUpdatedStreamEvent{NewAgent: st.agent}) {
		return nil, errConsumerStopped
	}

	for turn := st.startTurn; ; turn++ {
		// A graceful stop lands at the turn boundary: the finished turn's
		// tools and session save are in, so the run ends cleanly.
		if turn > st.startTurn && r.ctrl.stopRequested() {
			res := r.baseResult()
			res.StoppedEarly = true
			return res, nil
		}
		if r.maxTurns > 0 && turn > r.maxTurns {
			// FinalTurnWithoutTools grants THIS turn, tool-free and once; the
			// next overrun is the real end (spec §2.7d).
			if r.opts.Exec.ToolLoop.FinalTurnWithoutTools && !st.finalTurn {
				r.log.Info(ctx, "turn budget exhausted; one final turn without tools",
					slog.Int("max_turns", r.maxTurns))
				st.finalTurn = true
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
		// A cancellation at the turn boundary fails the run like any mid-loop
		// error, so completed turns reach the caller via RunError.Result.
		if err := ctx.Err(); err != nil {
			return nil, r.fail(err)
		}

		call, retry, err := r.runTurn(ctx, turn)
		if err != nil {
			return nil, err
		}
		if retry {
			// The context overflowed and compaction shortened it: run THIS
			// turn again — the budget counts calls the model got.
			turn--
			continue
		}

		preStep := r.generatedItems()
		step, err := r.executeToolsAndSideEffects(ctx, st.agent, call.processed, call.snapshot.OutputSchema, call.resumed, stepProgress{
			originalInput: st.originalInput,
			preStepItems:  preStep,
			resp:          call.resp,
		})
		if err != nil {
			return nil, r.fail(err)
		}
		// On a fresh turn NewStepItems begins with the model's own items, which
		// runTurn already emitted; a resumed turn holds side-effect items only.
		emitFrom := len(call.processed.NewItems)
		if call.resumed {
			emitFrom = 0
		}
		if !r.emitItems(step.NewStepItems[emitFrom:]) {
			return nil, errConsumerStopped
		}
		r.sessionItems = append(r.sessionItems, step.NewStepItems...)
		if len(call.processed.ToolsUsed) > 0 {
			r.markToolsUsed(st.agent)
		}
		// The server now holds what was sent plus the model's output; synthesized
		// items stay pending. A resumed turn's cursor was restored from RunState.
		if !call.resumed {
			st.cursor.advance(r.opts.Conversation, call.resp, len(preStep), len(call.processed.NewItems))
		}

		var a stepAction
		switch step.NextStep {
		case stepFinalOutput:
			a = r.handleFinalOutput(ctx, st, step)
		case stepHandoff:
			a = r.handleHandoff(ctx, st, step, call.snapshot, call.resp, turn)
		case stepInterruption:
			a = r.handleInterruption(ctx, step, call.resp, turn)
		case stepRunAgain:
			a = r.handleRunAgain(ctx, st, step, call.snapshot, call.resp, turn)
		}
		if a.done {
			return a.result, a.err
		}
	}
}

// turnCall is one turn's model response, classified and ready to execute.
type turnCall struct {
	snapshot  *TurnSnapshot
	resp      *ModelResponse
	processed *processedResponse
	// resumed marks a resume's first turn: the interrupted response is
	// re-processed, its items emitted and its usage counted before the pause.
	resumed bool
}

// runTurn takes a turn up to its classified model response. retry asks the
// loop to run the same turn again after an overflow recovery.
func (r *runner) runTurn(ctx context.Context, turn int) (call *turnCall, retry bool, err error) {
	st := r.state
	r.log.Debug(ctx, "turn started", slog.Int("turn", turn), slog.String("agent", st.agent.Name))

	if st.runStartHooks {
		if st.agent.OnStart != nil {
			if err := st.agent.OnStart(ctx, r.rc); err != nil {
				return nil, false, r.fail(err)
			}
		}
		st.runStartHooks = false
		// The agent span parents this agent's generation/tool spans.
		if r.agentSpan != nil {
			r.agentSpan.Finish()
		}
		r.agentSpan = r.trace.StartAgentSpan(st.agent.Name, r.opts.parentSpanID)
	}

	// Server-managed history gets only the items the server lacks, otherwise
	// the full history. A Blocking input guardrail's Replace rebuilds it below.
	turnInput, prevID, usedOriginalInput, err := r.buildTurnInput(st.cursor, st.originalInput, r.generatedItems())
	if err != nil {
		return nil, false, r.fail(err)
	}
	snapshot, err := r.buildSnapshot(ctx, st.agent, turnInput)
	if err != nil {
		return nil, false, r.fail(err)
	}
	// A snapshot a turn hook prepared replaces the resolved one — all but its
	// Input, which is the runner's (see TurnSnapshot).
	if st.pending != nil {
		st.pending.Input = turnInput
		snapshot = st.pending
		st.pending = nil
	}
	tools, handoffs := snapshot.Tools, snapshot.Handoffs
	modelInput := snapshot.Input
	if st.finalTurn {
		// Offered a tool the model would call one, and the budget would be
		// exhausted again with nothing said.
		tools, handoffs = nil, nil
	}
	// Input guardrails, hooks and tools all see exactly what the model is
	// sent; InputFilter may still edit it, in which case this is refreshed.
	r.rc.setTurnInput(modelInput)

	// First turn only: Blocking input guardrails gate the call, the rest race
	// it (run_input_guardrails.go). A resume or overflow retry already ran them.
	if turn == st.startTurn && r.resume == nil && !r.inputGuardrailsRan {
		r.inputGuardrailsRan = true
		gate, gerr := r.firstTurnInputGuardrails(ctx, st.agent, st.originalInput, usedOriginalInput, snapshot,
			func(replaced []InputItem) ([]InputItem, error) {
				in, _, _, err := r.buildTurnInput(st.cursor, replaced, r.generatedItems())
				return in, err
			})
		st.originalInput = gate.original
		r.inputRace = gate.race
		if gerr != nil {
			return nil, false, r.fail(gerr)
		}
		if gate.model != nil {
			modelInput = gate.model
		}
	}

	resp := st.pendingResponse
	resumed := resp != nil
	if resumed {
		st.pendingResponse = nil
	} else {
		out := r.callModelOnce(ctx, turn, snapshot, ModelRequest{
			SystemInstructions: snapshot.Instructions,
			Prompt:             snapshot.Prompt,
			Input:              modelInput,
			Settings:           snapshot.Settings,
			Tools:              tools,
			OutputSchema:       snapshot.OutputSchema,
			Handoffs:           handoffs,
			PreviousResponseID: prevID,
			ConversationID:     r.opts.Conversation.ConversationID,
		})
		if out.err != nil {
			return nil, false, out.err
		}
		if out.retry {
			return nil, true, nil
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
	// The settings the REQUEST carried, not the agent's: a turn hook may have
	// replaced the snapshot, and compaction reads Store off this.
	if snapshot.Settings != nil {
		r.lastStore = snapshot.Settings.Store
	} else {
		r.lastStore = nil
	}
	if !resumed {
		r.lastUsage = resp.Usage
		r.usagePending = true
	} else if r.resume != nil && r.resume.usagePending {
		// The pause withheld this response's items, so its usage debt transfers
		// to the resumed batch once — spec §2.7f.
		r.lastUsage = resp.Usage
		r.usagePending = true
	}

	processed, err := processModelResponse(st.agent, tools, handoffs, resp, r.opts.Exec.ToolNotFoundBehavior)
	if err != nil {
		return nil, false, r.fail(err)
	}
	// A resumed turn's items were emitted before the pause.
	if !resumed && !r.emitItems(processed.NewItems) {
		return nil, false, errConsumerStopped
	}
	return &turnCall{snapshot: snapshot, resp: resp, processed: processed, resumed: resumed}, false, nil
}

// stepAction is what loop() does after a NextStep handler runs: done=true
// returns (result, err) from the run; done=false continues the loop.
type stepAction struct {
	done   bool
	result *RunResult
	err    error
}

func loopAgain() stepAction { return stepAction{} }
func loopReturn(res *RunResult, err error) stepAction {
	return stepAction{done: true, result: res, err: err}
}

// handleFinalOutput ends the run, unless a late steer or queued follow-up
// continues it in the same trace/usage/session.
func (r *runner) handleFinalOutput(ctx context.Context, st *turnState, step *singleStepResult) stepAction {
	if extra := r.ctrl.takeContinuation(); len(extra) > 0 {
		// Appended before the closing write, so that write commits the take.
		injected := injectedInput(st.agent, extra)
		r.appendInjected(injected)
		if err := r.persistSessionItems(ctx); err != nil {
			return loopReturn(nil, r.fail(err))
		}
		if !r.emitItems(injected) {
			return loopReturn(nil, errConsumerStopped)
		}
		return loopAgain()
	}
	res, ferr := r.finishRun(ctx, step.FinalOutput)
	if ferr != nil {
		return loopReturn(nil, r.fail(ferr))
	}
	return loopReturn(res, nil)
}

// handleHandoff persists the turn, offers the stop hook, applies any input
// filter, then switches to the new agent.
func (r *runner) handleHandoff(ctx context.Context, st *turnState, step *singleStepResult, snapshot *TurnSnapshot, resp *ModelResponse, turn int) stepAction {
	// Persist before the input filter restarts the model's view: what is
	// stored is the whole log.
	if err := r.persistSessionItems(ctx); err != nil {
		return loopReturn(nil, r.fail(err))
	}
	// A handoff is a turn boundary; ask to stop before the filter runs so the
	// hook sees the turn as it happened.
	stop, out, serr := r.stopAfterTurn(ctx, st.agent, &TurnResult{
		Turn: turn, Response: resp, NewItems: step.NewStepItems, Snapshot: snapshot,
	})
	if serr != nil {
		return loopReturn(nil, r.fail(serr))
	}
	if stop {
		res, ferr := r.finishRun(ctx, out)
		if ferr != nil {
			return loopReturn(nil, r.fail(ferr))
		}
		return loopReturn(res, nil)
	}
	if step.Handoff != nil {
		if filter := r.handoffInputFilter(step.Handoff); filter != nil {
			// The server holds the unfiltered history; a filtered view desyncs.
			if r.opts.Conversation.UsePreviousResponseID || r.opts.Conversation.ConversationID != "" {
				err := NewUserError("handoff input filters (including NestHandoffHistory) are not supported with server-managed conversation state (UsePreviousResponseID / ConversationID)")
				return loopReturn(nil, r.fail(err))
			}
			filtered, ferr := applyHandoffInputFilter(filter, st.originalInput, r.generatedItems())
			if ferr != nil {
				return loopReturn(nil, r.fail(ferr))
			}
			st.originalInput = filtered
			r.restartGenerated()
			r.offChainHistory = true
		}
	}
	r.log.Info(ctx, "handoff",
		slog.String("from", st.agent.Name), slog.String("to", step.NewAgent.Name))
	st.agent = step.NewAgent
	st.runStartHooks = true
	if !r.emit(&AgentUpdatedStreamEvent{NewAgent: st.agent}) {
		return loopReturn(nil, errConsumerStopped)
	}
	return loopAgain()
}

// handleInterruption persists the completed part of the turn and returns the
// pause state; the injections it consumed ride in the persisted item log.
func (r *runner) handleInterruption(ctx context.Context, step *singleStepResult, resp *ModelResponse, turn int) stepAction {
	if err := r.persistSessionItems(ctx); err != nil {
		return loopReturn(nil, r.fail(err))
	}
	// Commit only after the persist succeeds: a failed attempt leaves the take
	// for finishStream to roll back and redeliver.
	r.ctrl.commitInjected()
	res := r.baseResult()
	res.Interruptions = step.Interruptions
	res.State = r.buildPauseState(turn, resp, step)
	return loopReturn(res, nil)
}

// handleRunAgain runs the save point and prepares the next turn: stop,
// recompact, inject follow-up input, or carry a prepared snapshot forward.
func (r *runner) handleRunAgain(ctx context.Context, st *turnState, step *singleStepResult, snapshot *TurnSnapshot, resp *ModelResponse, turn int) stepAction {
	sp, serr := r.savePoint(ctx, savePointInput{
		Turn:     turn,
		Agent:    st.agent,
		Snapshot: snapshot,
		Response: resp,
		NewItems: step.NewStepItems,
	})
	if serr != nil {
		return loopReturn(nil, r.fail(serr))
	}
	if sp.Stop {
		res, ferr := r.finishRun(ctx, sp.FinalOutput)
		if ferr != nil {
			return loopReturn(nil, r.fail(ferr))
		}
		return loopReturn(res, nil)
	}
	if sp.Recompacted {
		// The rebuilt context already holds this run's items, so the model's
		// view starts over.
		st.originalInput = sp.Input
		r.restartGenerated()
	}
	if len(sp.Injected) > 0 {
		r.appendInjected(sp.Injected)
		if !r.emitItems(sp.Injected) {
			return loopReturn(nil, errConsumerStopped)
		}
	}
	st.pending = sp.NextSnapshot
	return loopAgain()
}

// emitItems delivers each item to the stream, returning false when the consumer
// abandoned the run (the caller then returns errConsumerStopped).
func (r *runner) emitItems(items []*RunItem) bool {
	for _, it := range items {
		if !r.emitItem(it) {
			return false
		}
	}
	return true
}

// appendInjected records injected input on the log and advances the
// persist-boundary high-water; the caller emits and persists afterward.
func (r *runner) appendInjected(injected []*RunItem) {
	r.sessionItems = append(r.sessionItems, injected...)
	r.injectedUpTo = len(r.sessionItems)
}

// generatedItems is the model's view of the log, after originalInput. Clipped,
// so an appending caller reallocates instead of writing into the log.
func (r *runner) generatedItems() []*RunItem {
	return slices.Clip(r.sessionItems[r.generatedFrom:])
}

// restartGenerated empties the model's view of the log; the caller has folded
// the log so far into originalInput.
func (r *runner) restartGenerated() { r.generatedFrom = len(r.sessionItems) }

// modelCallOutcome is how one turn's model call ended: resp, retry (an
// overflow was compacted away), or err (fail-wrapped or errConsumerStopped).
type modelCallOutcome struct {
	resp  *ModelResponse
	retry bool
	err   error
}

// callModelOnce performs one turn's model call with everything around it: the
// user-input save, InputFilter, the span, the raced call, overflow recovery.
func (r *runner) callModelOnce(ctx context.Context, turn int, snap *TurnSnapshot, req ModelRequest) modelCallOutcome {
	st := r.state
	// The one-time user-input save lands here, not at loop start, so a failure
	// ahead of the first model call leaves no orphan user message (spec §2.5).
	if err := r.persistUserInput(ctx); err != nil {
		return modelCallOutcome{err: r.fail(err)}
	}
	if r.opts.Model.InputFilter != nil {
		edited, ferr := r.opts.Model.InputFilter(ctx, r.rc, st.agent,
			ModelInputData{Instructions: req.SystemInstructions, Input: req.Input})
		if ferr != nil {
			return modelCallOutcome{err: r.fail(ferr)}
		}
		req.SystemInstructions, req.Input = edited.Instructions, edited.Input
		// A snapshot is what the model was sent, so the edit lands on it too.
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
	call := func(ctx context.Context) (*ModelResponse, error) {
		ctx = tracing.WithSpan(ctx, span)
		if r.rawEvents {
			return r.streamOneModelCall(ctx, span, snap.Model, req)
		}
		return snap.Model.Respond(ctx, req)
	}
	var resp *ModelResponse
	var err error
	if race := r.inputRace; race != nil {
		// First-turn racing input guardrails watch the call from the side.
		out := r.raceModelCall(span, call, race, st.originalInput)
		// Release the race's contexts now, not at loop exit, or a long-lived
		// parent ctx keeps the registrations alive. Idempotent with stop.
		race.stop()
		r.inputRace = nil
		st.originalInput = out.original
		if out.stopped {
			return modelCallOutcome{err: errConsumerStopped}
		}
		if out.guardErr != nil {
			return modelCallOutcome{err: r.fail(out.guardErr)}
		}
		resp, err = out.resp, out.modelErr
	} else {
		resp, err = call(ctx)
	}
	if err != nil {
		span.SetError(err.Error(), nil)
		span.Finish()
		// The context did not fit: compact and retry the turn — spec §2.5g.
		if r.opts.Exec.Overflow.isOverflow(err) && r.overflowRetries < r.opts.Exec.Overflow.MaxRetries {
			if compacted, ok := r.recoverOverflow(ctx, err); ok {
				r.overflowRetries++
				st.originalInput = compacted
				r.restartGenerated()
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
// here or in another process.
func (r *runner) buildPauseState(turn int, resp *ModelResponse, step *singleStepResult) *RunState {
	// Under nestedMu: a timed-out tool's orphan goroutine may still be taking
	// nested states.
	r.rc.nestedMu.Lock()
	carriedNested := maps.Clone(r.rc.nestedToolStates)
	r.rc.nestedMu.Unlock()
	st := r.state
	return &RunState{
		CurrentAgent:          st.agent,
		OriginalInput:         st.originalInput,
		GeneratedItems:        r.generatedItems(),
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
		OffChainHistory:       r.offChainHistory,
		PendingInput:          r.ctrl.Pending(),
		DisclosedTools:        sortedKeys(r.disclosed),
		ReasoningItemIDPolicy: r.opts.Exec.ReasoningItemIDPolicy,
		cursor:                st.cursor,
		// First-turn input guardrails are not re-run on resume: this is the
		// only source of their results.
		GuardrailResults: r.snapshotGuardrailResults(),
		nestedToolStates: mergeNestedStates(carriedNested, step.NestedStates),
		usagePending:     r.usagePending,
	}
}
