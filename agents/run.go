package agents

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/zzir/agents-go/tracing"
)

// DefaultMaxTurns is the turn budget applied when RunOptions.MaxTurns is zero.
const DefaultMaxTurns = 10

// MaxTurnsUnlimited disables the turn budget when set as RunOptions.MaxTurns —
// the run loops until it produces a final output, hands off to a finishing
// agent, or is cancelled. The counterpart of Python's max_turns=None. Use with
// care: a model that never finishes will loop indefinitely.
const MaxTurnsUnlimited = -1

// ModelInputData is the editable portion of a model call passed to a
// CallModelInputFilter: the system instructions and the input items.
type ModelInputData struct {
	Instructions string
	Input        []TResponseInputItem
}

// CallModelInputFilter edits the instructions and input items just before a
// model call. Returning an error aborts the run.
type CallModelInputFilter func(ctx context.Context, rc *RunContext, agent *Agent, data ModelInputData) (ModelInputData, error)

// RunOptions configures a run. The zero value is valid: MaxTurns defaults to
// DefaultMaxTurns and a fresh RunContext is created. A ModelProvider (or an
// agent with an explicit ModelImpl) is required so the runner can obtain a Model.
type RunOptions struct {
	// MaxTurns bounds the number of model calls before the run aborts with a
	// MaxTurnsError. Zero means DefaultMaxTurns.
	MaxTurns int

	// Context is arbitrary user data threaded through tools, guardrails and
	// hooks via RunContext.Context. Ignored if RunContext is set.
	Context any

	// RunContext, when set, is used directly (and its Usage is accumulated into).
	// Otherwise a new one wrapping Context is created.
	RunContext *RunContext

	// ModelProvider resolves an agent's model name to a Model. Required unless
	// every agent in the run sets ModelImpl (or Model below is set).
	ModelProvider ModelProvider

	// Model overrides the model for every agent in the run, ignoring agent model
	// names. Takes precedence over ModelProvider lookups.
	Model Model

	// ModelSettings is a run-level settings override merged over each agent's
	// own ModelSettings.
	ModelSettings *ModelSettings

	// CallModelInputFilter, when set, is invoked just before each model call to
	// edit the instructions and input items sent (e.g. to trim tokens or inject
	// context). It does not change what is saved to the session.
	CallModelInputFilter CallModelInputFilter

	// MaxToolConcurrency bounds how many function tools run concurrently within a
	// single turn. Zero means no limit (every tool call in the turn runs in
	// parallel).
	MaxToolConcurrency int

	// ToolNotFoundBehavior controls what happens when the model calls a tool the
	// agent does not expose. The default (ToolNotFoundError) aborts the run;
	// ToolNotFoundReturnToModel feeds an error back so the model can retry.
	ToolNotFoundBehavior ToolNotFoundBehavior

	// PreApprovalToolInputGuardrails, when true, runs a tool's input guardrails
	// before surfacing a human-approval interruption for it: a guardrail
	// rejection returns the guardrail message as the tool output without
	// emitting an approval request or executing the tool. Calls that pass still
	// re-run the same guardrails immediately before execution after approval,
	// so time-sensitive checks are revalidated on resume. Off by default —
	// the counterpart of Python's
	// RunConfig.tool_execution.pre_approval_tool_input_guardrails.
	PreApprovalToolInputGuardrails bool

	// HandoffInputFilter is a run-level default applied to any handoff that does
	// not set its own Handoff.InputFilter. Use NestHandoffHistory to fold prior
	// history across all handoffs.
	HandoffInputFilter func(HandoffInputData) HandoffInputData

	// Guardrails apply to the whole run, in addition to each agent's own
	// Agent.Guardrails. Run-level guardrails are consulted first at every stage.
	Guardrails []Guardrail

	// ErrorHandlers supplies per-error-kind recovery handlers that can turn a
	// failing run — max turns exceeded, a model refusal, or an invalid
	// structured final output — into a normal completion with a fallback final
	// output. The zero value leaves every error fatal. The counterpart of
	// Python's Runner.run(..., error_handlers={...}).
	ErrorHandlers RunErrorHandlers

	// Hooks receives run-scoped lifecycle callbacks.
	Hooks RunHooks

	// Session, when set, supplies and persists conversation history: prior items
	// are prepended to the input, and the new input plus generated items are
	// saved after the run completes.
	Session Session

	// SessionInputCallback customizes how stored session history is combined with
	// the run's new input. Nil (the default) appends new input to history; a
	// custom callback may reorder, filter or fold history. Only genuinely new
	// items are persisted back to the session. Ignored without a Session — the
	// counterpart of Python's RunConfig.session_input_callback.
	SessionInputCallback SessionInputCallback

	// SessionSettings overrides how the run reads the Session (e.g. how many
	// recent items to load). Non-zero fields take precedence over a Session-level
	// default. Ignored without a Session — the counterpart of Python's
	// RunConfig.session_settings.
	SessionSettings *SessionSettings

	// ReasoningItemIDPolicy controls whether reasoning-item ids are kept when run
	// items are converted back into model input on later turns. The default
	// (ReasoningItemIDPreserve) keeps them; ReasoningItemIDOmit strips them. It is
	// persisted across interruptions in RunState — the counterpart of Python's
	// RunConfig.reasoning_item_id_policy.
	ReasoningItemIDPolicy ReasoningItemIDPolicy

	// Tracer, when set, records a trace of the run with a span per model call.
	// Build one with tracing.NewTracer(processor).
	Tracer *tracing.Tracer

	// TraceIncludeSensitiveData controls whether generation spans record the
	// full model request (model, system instructions, input items) and output
	// items. nil reads the OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA
	// environment variable, where anything but "false" means true — matching
	// the Python SDK's RunConfig.trace_include_sensitive_data default. Set to
	// false when trace exports must not carry conversation content.
	TraceIncludeSensitiveData *bool

	// TraceGroupID links this run's trace to a group of related traces (e.g.
	// one chat thread across several runs) — the counterpart of Python's
	// RunConfig.group_id. Only used when Tracer starts a new trace.
	TraceGroupID string

	// TraceMetadata attaches user metadata to the run's trace — the
	// counterpart of Python's RunConfig.trace_metadata. Only used when Tracer
	// starts a new trace.
	TraceMetadata map[string]any

	// UsePreviousResponseID opts into server-managed conversation state: instead
	// of resending the full history each turn, the runner chains calls via the
	// OpenAI Responses API's previous_response_id and sends only new items. This
	// saves tokens but requires a model that returns response IDs and keeps
	// responses stored (the default; do not set ModelSettings.Store=false).
	UsePreviousResponseID bool

	// ConversationID attaches the run to a server-side OpenAI conversation
	// (the Responses API `conversation` parameter). Like UsePreviousResponseID,
	// the server holds history, so the runner sends only new items each turn. It
	// is server-managed state and must not be combined with a local Session.
	ConversationID string

	// parentTrace, when set, makes the run record its spans into an existing
	// trace instead of starting (and finishing) its own. Set internally for
	// nested agent-as-tool runs; not user-facing.
	parentTrace *tracing.TraceHandle

	// parentSpanID, when set alongside parentTrace, parents the nested run's
	// agent spans under the function span of the agent-as-tool call that
	// triggered it, so trace trees show which tool call owns the nested run.
	// Set internally; not user-facing.
	parentSpanID string
}

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
func Run(ctx context.Context, agent *Agent, input any, opts RunOptions) (RunStream, RunControl) {
	ctrl := newRunControl()
	return func(yield func(StreamEvent, error) bool) {
		runStream(ctx, agent, input, opts, ctrl, true, yield)
	}, ctrl
}

// RunSync executes a run to completion and returns its result. It is Run
// without the stream: the model is called without streaming, and no raw model
// events are produced.
//
// It is the entry point to reach for unless you need to observe a run as it
// happens.
func RunSync(ctx context.Context, agent *Agent, input any, opts RunOptions) (*RunResult, error) {
	ctrl := newRunControl()
	stream := RunStream(func(yield func(StreamEvent, error) bool) {
		runStream(ctx, agent, input, opts, ctrl, false, yield)
	})
	return stream.Collect()
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

	res, err := r.loop(ctx, agent, modelInput)
	r.finishStream(res, err)
}

// finishStream reports a completed loop to the consumer. A consumer that
// already stopped is told nothing further — yield has returned false, so there
// is nobody listening.
func (r *runner) finishStream(res *RunResult, err error) {
	r.ctrl.setPhase(PhaseIdle)
	if r.consumerStopped || errors.Is(err, errConsumerStopped) {
		return
	}
	if err != nil {
		r.yield(nil, err)
		return
	}
	r.yield(&RunCompletedEvent{Result: res}, nil)
}

// prepareRun builds the runner shared by Run and RunStreamed: it normalizes
// the input, validates server-state options, wires the run context, starts
// (or joins) the trace and prepends session history to the model input. The
// returned finish func ends the trace — a no-op when the trace was joined
// (nested run) or tracing is off — and must be deferred by the caller.
// ResumeRun has its own entry construction (it seeds from a RunState) and
// shares only the loop.
func prepareRun(ctx context.Context, agent *Agent, input any, opts RunOptions) (*runner, []TResponseInputItem, func(), error) {
	maxTurns := opts.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}
	// A negative value (MaxTurnsUnlimited) disables the budget; it passes
	// through here and the turn check skips it.

	rc := opts.RunContext
	if rc == nil {
		rc = NewRunContext(opts.Context)
	}
	if rc.Usage == nil {
		rc.Usage = NewUsage()
	}
	rc.inheritedOpts = &opts

	userInput, err := normalizeInput(input)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateServerState(opts); err != nil {
		return nil, nil, nil, err
	}

	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, userInput: userInput}

	finishTrace := func() {}
	if opts.parentTrace != nil {
		// Nested run (agent-as-tool): record spans into the parent's trace
		// rather than starting an orphan root trace. The parent finishes it.
		r.trace = opts.parentTrace
	} else if opts.Tracer != nil {
		workflow := agent.Name
		if workflow == "" {
			workflow = "Agent workflow"
		}
		var topts []tracing.TraceOption
		if opts.TraceGroupID != "" {
			topts = append(topts, tracing.WithGroupID(opts.TraceGroupID))
		}
		if opts.TraceMetadata != nil {
			topts = append(topts, tracing.WithMetadata(opts.TraceMetadata))
		}
		r.trace = opts.Tracer.StartTrace(workflow, topts...)
		finishTrace = r.trace.Finish
	}
	rc.activeTrace = r.trace

	// With a session, prepend stored history to the model input. A
	// SessionInputCallback may instead reorder or fold history; when it does,
	// only the genuinely new items are persisted (r.userInput is narrowed).
	modelInput := userInput
	if opts.Session != nil {
		limit := resolveSessionLimit(opts.SessionSettings, opts.Session)
		history, herr := opts.Session.GetItems(ctx, limit)
		if herr != nil {
			return nil, nil, nil, herr
		}
		if opts.SessionInputCallback != nil {
			combined, cerr := opts.SessionInputCallback(history, userInput)
			if cerr != nil {
				return nil, nil, nil, cerr
			}
			// Persistence diffs against the raw combined so the callback's chosen
			// new items are saved intact, but the model input is scrubbed just like
			// the default branch below: a callback that folds history can carry a
			// dangling tool call or a duplicate that would otherwise 400 at the
			// Responses API.
			r.userInput = sessionAppendedItems(history, userInput, combined)
			modelInput = normalizeStoredInput(combined)
		} else if len(history) > 0 {
			modelInput = make([]TResponseInputItem, 0, len(history)+len(userInput))
			modelInput = append(modelInput, history...)
			modelInput = append(modelInput, userInput...)
			// Scrub the merged history+input before it reaches the model: a
			// stored dangling tool call (e.g. persisted by the Python SDK at an
			// interruption) or a duplicate re-sent item would otherwise 400 at
			// the Responses API. Mirrors Python's prepare_input_with_session.
			modelInput = normalizeStoredInput(modelInput)
		}
	}

	return r, modelInput, finishTrace, nil
}

// modelCallOutcome carries a model call's result off a goroutine when the call
// races the first-turn input guardrails (see the loop's guardCh path).
type modelCallOutcome struct {
	resp *ModelResponse
	err  error
}

// inputGuardOutcome carries the parallel input guardrails' collected results and
// tripwire/error off their goroutine, so the main loop can both honor the
// tripwire and record every result on the RunResult.
type inputGuardOutcome struct {
	results []GuardrailResult
	err     error
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
	// loop checks it wherever it checks for cancellation.
	consumerStopped bool

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

	// cancelInputGuardrails, when set, cancels the in-flight input-guardrail
	// goroutine. Deferred so every early exit (e.g. a failed model call) stops
	// an LLM-based guardrail instead of letting it run to completion after the
	// run has already returned.
	var cancelInputGuardrails context.CancelFunc
	defer func() {
		if cancelInputGuardrails != nil {
			cancelInputGuardrails()
		}
	}()

	currentAgent := startAgent
	generatedItems := []RunItem{}
	rawResponses := []*ModelResponse{}
	shouldRunStartHooks := true
	startTurn := 1

	// When resuming from an interruption, seed prior state and re-process the
	// captured response on the first iteration instead of calling the model.
	var pendingResponse *ModelResponse
	if r.resume != nil {
		currentAgent = r.resume.CurrentAgent
		originalInput = r.resume.OriginalInput
		generatedItems = append([]RunItem{}, r.resume.GeneratedItems...)
		rawResponses = append([]*ModelResponse{}, r.resume.RawResponses...)
		pendingResponse = r.resume.InterruptedResponse
		sessionSeed := r.resume.SessionItems
		if sessionSeed == nil {
			sessionSeed = r.resume.GeneratedItems
		}
		r.sessionItems = append([]RunItem{}, sessionSeed...)
		// The interrupted run already persisted its user input and every turn up
		// to the pause (holding back the pending, output-less tool calls), so the
		// resume continues from that cursor instead of re-saving.
		r.persistedSessionItems = r.resume.PersistedSessionItems
		r.userInputSaved = true
		// Continue counting turns where the interrupted run stopped, so repeated
		// interrupt/resume cycles cannot exceed the turn budget.
		if r.resume.CurrentTurn > 1 {
			startTurn = r.resume.CurrentTurn
		}
	}

	// previous_response_id tracking: previousResponseID chains server-side
	// conversation state; serverItemCount marks how many generated items the
	// server already has (so we only send the rest).
	var previousResponseID string
	var serverItemCount int
	var serverCursorActive bool

	// Persist the new user input up front (original run only; a resume's input
	// was saved before it paused). Mirrors Python persisting input before the
	// Runs defer the one-time user-input save to just before the first model
	// call (see below), so a failure ahead of that — a blocking input-guardrail
	// tripwire, a bad model config — leaves no orphan user message behind.

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
				Usage:            r.rc.Usage,
				GuardrailResults: r.snapshotGuardrailResults(),
			}, nil
		}
		if r.maxTurns > 0 && turn > r.maxTurns {
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Publish the live turn/agent for RunControl (guarded by a mutex; the
		// run loop and the caller race).
		r.ctrl.setCurrent(currentAgent, turn)

		if shouldRunStartHooks {
			if err := callAgentStart(ctx, r.opts.Hooks, currentAgent, r.rc); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			shouldRunStartHooks = false
			// Start an agent span (parent of this agent's generation/tool spans).
			if r.agentSpan != nil {
				r.agentSpan.Finish()
			}
			r.agentSpan = r.trace.StartAgentSpan(currentAgent.Name, r.opts.parentSpanID)
		}

		model, err := r.resolveModel(currentAgent)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		systemPrompt, err := currentAgent.GetSystemPrompt(ctx, r.rc)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		prompt, err := currentAgent.GetPrompt(ctx, r.rc)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		outputSchema := agentOutputSchema(currentAgent)
		if err := outputSchemaError(outputSchema); err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		handoffs, err := r.enabledHandoffs(ctx, currentAgent)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		tools, err := r.enabledTools(ctx, currentAgent)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		// Build the model input. In previous_response_id mode, send only the
		// items the server does not yet have; otherwise send the full history.
		var modelInput []TResponseInputItem
		var prevID string
		switch {
		case r.opts.UsePreviousResponseID && previousResponseID != "":
			modelInput, err = itemsToInputList(generatedItems[serverItemCount:])
			prevID = previousResponseID
		case r.opts.ConversationID != "" && serverCursorActive:
			// The conversation already holds prior items server-side.
			modelInput, err = itemsToInputList(generatedItems[serverItemCount:])
		default:
			modelInput, err = buildModelInput(originalInput, generatedItems)
		}
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		// Optionally strip reasoning-item ids before sending them to the model.
		modelInput = applyReasoningItemIDPolicy(modelInput, r.opts.ReasoningItemIDPolicy)
		// Publish the turn's input so input guardrails, hooks and tools all see
		// exactly what the model is being sent. CallModelInputFilter may still
		// edit it below, in which case this is refreshed.
		r.rc.setTurnInput(modelInput)

		// On the first turn, run input guardrails (run-level ones first, then the
		// starting agent's). A guardrail with Blocking=true runs to
		// completion BEFORE the model call — a gate. The rest run
		// concurrently with the model call for blocking runs (matching the Python
		// SDK), synchronously before it for streaming runs (the documented
		// difference); guardCh delivers their results and tripwire. They already
		// ran before an interruption, so a resumed run skips them.
		var guardCh chan inputGuardOutcome
		if turn == startTurn && r.resume == nil {
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
					GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: originalInput})
				r.recordGuardrailResults(res...)
				if repl, ok := inputReplacement(res); ok {
					originalInput = repl
				}
				if gerr != nil {
					gspan.SetError(gerr.Error(), nil)
					gspan.Finish()
					return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				gspan.Finish()
			}
			// Non-blocking guardrails race the model call: a tripwire cancels it,
			// so the run fails without waiting for a response nobody will use.
			//
			// This used to be the blocking entry point's behavior only —
			// streaming ran them synchronously first, which is what the guardrail
			// docs called "the documented difference". There is one entry point
			// now, and racing is the behavior the docs describe.
			if len(parallel) > 0 {
				guardCh = make(chan inputGuardOutcome, 1)
				gctx, gcancel := context.WithCancel(ctx)
				cancelInputGuardrails = gcancel
				parentID := r.agentParentID() // read before the goroutine races a handoff
				go func() {
					gspan := r.trace.StartGuardrailSpan("input", parentID)
					res, gerr := runStageConcurrent(gctx, r.rc, parallel,
						GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: originalInput})
					if gerr != nil {
						gspan.SetError(gerr.Error(), nil)
					}
					gspan.Finish()
					guardCh <- inputGuardOutcome{results: res, err: gerr}
				}()
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
			if r.opts.CallModelInputFilter != nil {
				edited, ferr := r.opts.CallModelInputFilter(ctx, r.rc, currentAgent, ModelInputData{Instructions: systemPrompt, Input: modelInput})
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				systemPrompt, modelInput = edited.Instructions, edited.Input
				r.rc.setTurnInput(modelInput)
			}
			if err := callLLMStart(ctx, r.opts.Hooks, currentAgent, r.rc, systemPrompt, modelInput); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			req := ModelRequest{
				SystemInstructions: systemPrompt,
				Prompt:             prompt,
				Input:              modelInput,
				Settings:           r.resolveSettings(currentAgent),
				Tools:              tools,
				OutputSchema:       outputSchema,
				Handoffs:           handoffs,
				Tracing:            ModelTracingDisabled,
				PreviousResponseID: prevID,
				ConversationID:     r.opts.ConversationID,
			}
			span := r.startGenerationSpan(currentAgent, req)
			switch {
			case guardCh != nil:
				// Blocking run with first-turn parallel input guardrails: race the
				// model call against them so a tripwire cancels the in-flight call.
				// A tripped guardrail aborts the turn WITHOUT billing usage or
				// firing OnLLMEnd — the model task is discarded (Python parity:
				// should_cancel_parallel_model_task_on_input_guardrail_trip).
				modelCtx, modelCancel := context.WithCancel(ctx)
				ch := make(chan modelCallOutcome, 1)
				go func() {
					rr, ee := model.GetResponse(modelCtx, req)
					ch <- modelCallOutcome{resp: rr, err: ee}
				}()
				var tripwire error
				readGuard := func(g inputGuardOutcome) {
					r.recordGuardrailResults(g.results...)
					tripwire = g.err
				}
				select {
				case g := <-guardCh:
					readGuard(g)
					if tripwire == nil {
						out := <-ch
						resp, err = out.resp, out.err
					}
				case out := <-ch:
					// Model finished first; honor a tripwire verdict still in flight.
					readGuard(<-guardCh)
					if tripwire == nil {
						resp, err = out.resp, out.err
					}
				}
				modelCancel()
				if tripwire != nil {
					span.SetError(tripwire.Error(), nil)
					span.Finish()
					return nil, r.fail(tripwire, originalInput, generatedItems, rawResponses, currentAgent)
				}
			case r.rawEvents:
				resp, err = r.streamOneModelCall(ctx, span, model, req)
			default:
				resp, err = model.GetResponse(ctx, req)
			}
			if err != nil {
				span.SetError(err.Error(), nil)
				span.Finish()
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			r.finishGenerationSpan(span, resp)
			// The model call completed and any first-turn input guardrails passed,
			// so bill usage and surface the response to OnLLMEnd.
			r.rc.Usage.Add(resp.Usage)
			rawResponses = append(rawResponses, resp)
			if err := callLLMEnd(ctx, r.opts.Hooks, currentAgent, r.rc, resp); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
		}
		r.lastResponseID = resp.ResponseID
		r.lastStore = r.resolveSettings(currentAgent).Store

		processed, err := processModelResponse(currentAgent, tools, handoffs, resp, r.opts.ToolNotFoundBehavior)
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

		// Advance the server cursor: items the server already has are everything
		// sent this turn plus the model's own output items; synthesized items
		// (tool outputs) remain pending for the next call.
		if r.opts.UsePreviousResponseID && resp.ResponseID != "" {
			previousResponseID = resp.ResponseID
			if resumedTurn {
				// The interrupted response's own items were already recorded
				// before the run paused; only this turn's tool outputs pend.
				serverItemCount = lenBeforeStep
			} else {
				serverItemCount = lenBeforeStep + len(processed.NewItems)
			}
		}
		// conversation_id mode: the server appends each turn's items, so advance
		// the cursor and send only deltas from the next turn on.
		if r.opts.ConversationID != "" {
			serverCursorActive = true
			if resumedTurn {
				serverItemCount = lenBeforeStep
			} else {
				serverItemCount = lenBeforeStep + len(processed.NewItems)
			}
		}

		switch step.NextStep {
		case stepFinalOutput:
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
			if err := callHandoff(ctx, r.opts.Hooks, currentAgent, step.NewAgent, r.rc); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if step.Handoff != nil {
				if filter := r.handoffInputFilter(step.Handoff); filter != nil {
					// A handoff input filter cannot coexist with server-managed
					// conversation state: the server holds the unfiltered history,
					// so a filtered view would desync (in ConversationID mode,
					// resending the full filtered input duplicates the server's
					// stored items). Fail fast, matching Python's UserError.
					if r.opts.UsePreviousResponseID || r.opts.ConversationID != "" {
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
			currentAgent = step.NewAgent
			shouldRunStartHooks = true
			if !r.emit(&AgentUpdatedStreamEvent{NewAgent: currentAgent}) {
				return nil, errConsumerStopped
			}
			continue
		case stepInterruption:
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
				Usage:                 r.rc.Usage,
				CurrentTurn:           turn,
				MaxTurns:              r.maxTurns,
				ToolsUsed:             toolsUsedList(r.toolsUsedBy),
				ReasoningItemIDPolicy: r.opts.ReasoningItemIDPolicy,
				// Carry the guardrail results accumulated so far so a resumed run's
				// RunResult still reports them: first-turn input guardrails are not
				// re-run on resume (Python parity), so this is their only source.
				GuardrailResults: r.snapshotGuardrailResults(),
				// Carry any paused agent-as-tool nested states so ResumeRun
				// continues them; merge with any already cached on the run context
				// from an earlier resume of the same parent run. Serialized in
				// RunState JSON, so a cross-process resume continues them too.
				nestedToolStates: mergeNestedStates(carriedNested, step.NestedStates),
			}
			return &RunResult{
				Input: originalInput,
				// Unfiltered log for observability (State.GeneratedItems keeps the
				// filtered view for resume correctness).
				NewItems:         r.sessionItems,
				RawResponses:     rawResponses,
				LastAgent:        currentAgent,
				Usage:            r.rc.Usage,
				GuardrailResults: r.snapshotGuardrailResults(),
				Interruptions:    step.Interruptions,
				State:            state,
			}, nil
		case stepRunAgain:
			// Persist the just-completed turn (all tool calls have their outputs)
			// before looping, so a later cancel keeps this turn's work.
			if err := r.persistSessionItems(ctx); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			continue
		}
	}
}

// finishRun is the final-output tail shared by the normal completion path and
// a max-turns recovery. Order: the agent-end hook fires FIRST (matching Python;
// before output guardrails — a tripped
// guardrail does not suppress on_agent_end), then output guardrails, then
// session persistence and compaction (kept after guardrails, matching Python's
// streaming path: a guardrail-tripped final output is not persisted).
func (r *runner) finishRun(ctx context.Context, agent *Agent, originalInput []TResponseInputItem, raw []*ModelResponse, finalOutput any) (*RunResult, error) {
	if err := callAgentEnd(ctx, r.opts.Hooks, agent, r.rc, finalOutput); err != nil {
		return nil, err
	}
	// Output guardrails: run-level ones first, then the producing agent's.
	// A Replace decision substitutes the final output and the run continues.
	if outGuardrails := selectStage(r.runGuardrails(agent), StageOutput); len(outGuardrails) > 0 {
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
	r.maybeCompact(ctx)
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
	rec, err := r.resolveErrorRecovery(ctx, r.opts.ErrorHandlers.MaxTurns, cause, agent, originalInput, r.sessionItems, rawResponses)
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
	}
	var ae *AgentsError
	if asAgentsError(err, &ae) {
		ae.Details = details
		return err
	}
	return err
}

// persistUserInput writes the run's new user input to the session once, at loop
// start. Later per-turn saves persist only generated items, so the prompt is
// never rewritten. No-op without a session or when there is no new input.
func (r *runner) persistUserInput(ctx context.Context) error {
	if r.opts.Session == nil || r.userInputSaved || len(r.userInput) == 0 {
		return nil
	}
	if err := r.opts.Session.AddItems(ctx, r.userInput); err != nil {
		return err
	}
	r.userInputSaved = true
	return nil
}

// persistSessionItems incrementally saves the sessionItems produced since the
// last save. It persists only the "safe" leading prefix: a trailing
// function_call still awaiting its output (a HITL pause) is held back so the
// stored conversation never contains a call without its output, which would be
// rejected on replay. The held-back calls save on the next turn, once their
// outputs arrive. sessionItems is the unfiltered log, so handoff input filters
// never affect what is stored.
func (r *runner) persistSessionItems(ctx context.Context) error {
	if r.opts.Session == nil {
		return nil
	}
	end := safePersistBoundary(r.sessionItems, r.persistedSessionItems)
	if end <= r.persistedSessionItems {
		return nil
	}
	toSave, err := itemsToInputList(r.sessionItems[r.persistedSessionItems:end])
	if err != nil {
		return err
	}
	if len(toSave) > 0 {
		if err := r.opts.Session.AddItems(ctx, toSave); err != nil {
			return err
		}
	}
	r.persistedSessionItems = end
	return nil
}

// safePersistBoundary returns the exclusive end index up to which items[start:]
// can be safely persisted without ever storing a function_call that lacks its
// matching function_call_output. It returns the largest end such that every
// function_call in items[start:end] has its output also within items[start:end).
//
// Scanning left to right, the boundary advances to just past each point where no
// call is left open (awaiting its output). A pending call — and everything
// ordered after it, including a completed sibling's output that happens to sit
// after it (as at a nested agent-as-tool pause: [call S, call A(pending),
// output S]) — is held back until the missing outputs arrive on resume, so the
// stored history never contains a dangling call. A turn whose calls are all
// paired therefore persists in full.
func safePersistBoundary(items []RunItem, start int) int {
	if start >= len(items) {
		return len(items)
	}
	end := start
	open := map[string]struct{}{}
	for i := start; i < len(items); i++ {
		id, isCall, isOutput := runItemCallID(items[i])
		switch {
		case isCall:
			open[id] = struct{}{}
		case isOutput:
			delete(open, id)
		}
		if len(open) == 0 {
			end = i + 1
		}
	}
	return end
}

// runItemCallID reports a run item's function-call correlation id and whether it
// is a call or an output, by inspecting its input-item form. Non-function items
// (messages, reasoning, handoffs) report isCall=isOutput=false. Works uniformly
// for live items and items rebuilt from serialized RunState.
func runItemCallID(it RunItem) (callID string, isCall, isOutput bool) {
	in, err := it.ToInputItem()
	if err != nil {
		return "", false, false
	}
	switch {
	case in.OfFunctionCall != nil:
		return in.OfFunctionCall.CallID, true, false
	case in.OfFunctionCallOutput != nil:
		return in.OfFunctionCallOutput.CallID, false, true
	}
	return "", false, false
}

// maybeCompact gives a self-compacting session a chance to compact now that the
// run's items are persisted and the final output is produced. It is
// best-effort housekeeping: a failure is recorded on the trace instead of
// turning the successful run into an error. Called once, at final output — Go
// compacts per run, not per turn (see docs/migration_from_python.md).
func (r *runner) maybeCompact(ctx context.Context) {
	if r.opts.Session == nil {
		return
	}
	// Items produced locally AFTER the last model response — a final turn's
	// tool/handoff outputs (StopOnFirstTool, rejected calls) or a synthesized
	// error-handler fallback message — are not on the server's
	// previous_response_id chain, so compacting from lastResponseID would
	// erase them from the stored history. Python defers compaction for such
	// turns (save_result_to_session's has_local_tool_outputs check); with one
	// compaction per run, ending on such a turn means skipping it.
	if endsWithLocalItem(r.sessionItems) {
		return
	}
	if cs, ok := r.opts.Session.(CompactionAwareSession); ok {
		// The span starts lazily — only when the session actually compacts —
		// so no-op passes don't clutter the trace.
		var cspan *tracing.SpanHandle
		cerr := cs.RunCompaction(ctx, CompactionArgs{
			ResponseID: r.lastResponseID,
			Store:      r.lastStore,
			StartSpan: func() *tracing.SpanHandle {
				cspan = r.trace.StartCompactionSpan(r.agentParentID())
				return cspan
			},
		})
		if cerr != nil && cspan == nil {
			// Failed before the session opened the span; open one so the
			// error is still visible on the trace.
			cspan = r.trace.StartCompactionSpan(r.agentParentID())
		}
		if cspan != nil {
			if cerr != nil {
				cspan.SetError(cerr.Error(), nil)
			}
			cspan.Finish()
		}
	}
}

// endsWithLocalItem reports whether the run's last item was produced locally
// by the SDK rather than returned by the model: a tool/handoff output, or an
// error-handler's synthesized fallback message (marked with the fake response
// id). Such items postdate the last model response and are absent from the
// server-side response chain that previous_response_id compaction replays.
func endsWithLocalItem(items []RunItem) bool {
	if len(items) == 0 {
		return false
	}
	switch it := items[len(items)-1].(type) {
	case *ToolCallOutputItem, *HandoffOutputItem:
		return true
	case *MessageOutputItem:
		return it.Raw.ID == fakeResponsesID
	case *rawInputRunItem:
		return it.Kind == "tool_call_output" || it.Kind == "handoff_output"
	}
	return false
}

// mergeNestedStates combines any agent-as-tool nested states still cached on
// the run context (un-consumed from a prior resume) with those freshly paused
// this turn, preferring the fresh ones. Returns nil when both are empty so a
// run without nested-tool HITL carries no map.
func mergeNestedStates(carried, fresh map[string]*RunState) map[string]*RunState {
	if len(carried) == 0 && len(fresh) == 0 {
		return nil
	}
	out := make(map[string]*RunState, len(carried)+len(fresh))
	maps.Copy(out, carried)
	maps.Copy(out, fresh)
	return out
}

// validateServerState rejects incompatible server-managed conversation options.
// conversation_id and previous_response_id both put history on the server, so
// they cannot be combined with each other or with a local Session.
func validateServerState(opts RunOptions) error {
	if opts.ConversationID != "" {
		if opts.UsePreviousResponseID {
			return newUserError("ConversationID cannot be combined with UsePreviousResponseID")
		}
		if opts.Session != nil {
			return newUserError("ConversationID cannot be combined with a local Session")
		}
	}
	if opts.UsePreviousResponseID && opts.Session != nil {
		return newUserError("UsePreviousResponseID cannot be combined with a local Session")
	}
	return nil
}

// toolsUsedList returns the agent names in a tool-use tracker as a slice, for
// carrying the tool_choice reset across an interrupt/resume in RunState.
func toolsUsedList(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}

// markToolsUsed records that agent called tools this run (for tool_choice reset).
func (r *runner) markToolsUsed(agent *Agent) {
	if r.toolsUsedBy == nil {
		r.toolsUsedBy = map[string]bool{}
	}
	r.toolsUsedBy[agent.Name] = true
}

// resolveModel returns the Model for the given agent, honoring (in order) the
// agent's explicit ModelImpl, the run-level Model override, then the provider.
// setGenerationUsage records a single model call's token counts on its
// generation span, so trace consumers see per-call input/output/total tokens
// (rc.Usage holds the run-wide accumulation separately).
func setGenerationUsage(span *tracing.SpanHandle, u *Usage) {
	if u == nil {
		return
	}
	span.Set("input_tokens", u.InputTokens)
	span.Set("output_tokens", u.OutputTokens)
	span.Set("total_tokens", u.TotalTokens)
}

// traceIncludeSensitiveData resolves RunOptions.TraceIncludeSensitiveData,
// falling back to the OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA environment
// variable where anything but "false" means true — the same default as the
// Python SDK.
func (r *runner) traceIncludeSensitiveData() bool {
	if r.opts.TraceIncludeSensitiveData != nil {
		return *r.opts.TraceIncludeSensitiveData
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA")), "false")
}

// traceTools projects the request's tools into a serializable form — the
// function fields on FunctionTool cannot be marshaled.
func traceTools(tools []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		m := map[string]any{"name": t.ToolName()}
		if ft, ok := t.(*FunctionTool); ok {
			if ft.Description != "" {
				m["description"] = ft.Description
			}
			if ft.ParamsJSONSchema != nil {
				m["parameters"] = ft.ParamsJSONSchema
			}
		}
		out = append(out, m)
	}
	return out
}

// traceHandoffs projects the request's handoffs into a serializable form.
func traceHandoffs(handoffs []Handoff) []map[string]any {
	out := make([]map[string]any, 0, len(handoffs))
	for _, h := range handoffs {
		out = append(out, map[string]any{
			"tool_name":  h.ToolName,
			"agent_name": h.AgentName,
		})
	}
	return out
}

// startGenerationSpan opens the span for one model call and, unless
// sensitive-data tracing is off, records the full request body: model name,
// system instructions, input items, tool definitions, model settings (the
// Extra* passthrough fields are excluded by their json tags), handoffs, and
// output schema. Slices are cloned or projected because exporters serialize
// asynchronously, after the runner may have modified the originals.
func (r *runner) startGenerationSpan(agent *Agent, req ModelRequest) *tracing.SpanHandle {
	span := r.trace.StartGenerationSpan(agent.Name, r.agentParentID())
	if span.Span == nil || !r.traceIncludeSensitiveData() {
		return span
	}
	if agent.Model != "" {
		span.Set("model", agent.Model)
	}
	if req.SystemInstructions != "" {
		span.Set("system_instructions", req.SystemInstructions)
	}
	span.Set("input", slices.Clone(req.Input))
	if len(req.Tools) > 0 {
		span.Set("tools", traceTools(req.Tools))
	}
	if len(req.Handoffs) > 0 {
		span.Set("handoffs", traceHandoffs(req.Handoffs))
	}
	if req.Settings != nil {
		span.Set("model_settings", *req.Settings)
	}
	if req.OutputSchema != nil && !req.OutputSchema.IsPlainText() {
		span.Set("output_schema", map[string]any{
			"name":   req.OutputSchema.Name(),
			"schema": req.OutputSchema.JSONSchema(),
		})
	}
	if req.Prompt != nil {
		span.Set("prompt", *req.Prompt)
	}
	if req.PreviousResponseID != "" {
		span.Set("previous_response_id", req.PreviousResponseID)
	}
	if req.ConversationID != "" {
		span.Set("conversation_id", req.ConversationID)
	}
	return span
}

// finishGenerationSpan records the model call's outcome — response id, usage,
// and (unless sensitive-data tracing is off) the output items — and ends the
// span.
func (r *runner) finishGenerationSpan(span *tracing.SpanHandle, resp *ModelResponse) {
	span.Set("response_id", resp.ResponseID)
	setGenerationUsage(span, resp.Usage)
	if span.Span != nil && r.traceIncludeSensitiveData() {
		span.Set("output", slices.Clone(resp.Output))
	}
	span.Finish()
}

func (r *runner) resolveModel(agent *Agent) (Model, error) {
	if agent.ModelImpl != nil {
		return agent.ModelImpl, nil
	}
	if r.opts.Model != nil {
		return r.opts.Model, nil
	}
	if r.opts.ModelProvider != nil {
		return r.opts.ModelProvider.GetModel(agent.Model)
	}
	return nil, newUserError("no model available: set Agent.ModelImpl, RunOptions.Model, or RunOptions.ModelProvider")
}

// resolveSettings merges the run-level settings override over the agent's own.
func (r *runner) resolveSettings(agent *Agent) *ModelSettings {
	base := agent.ModelSettings
	if base == nil {
		base = &ModelSettings{}
	}
	s := base.Resolve(r.opts.ModelSettings)
	// Once an agent has called tools, leave tool_choice unset on its later
	// turns so a "required"/specific-tool setting cannot force an infinite
	// tool-call loop (the Python SDK's reset_tool_choice behavior).
	if !agent.DisableToolChoiceReset && s.ToolChoice != "" && r.toolsUsedBy[agent.Name] {
		s.ToolChoice = ""
	}
	return s
}

// enabledTools returns the agent's tools, filtered by any IsEnabled predicate
// and augmented with the tools exposed by the agent's MCP servers.
func (r *runner) enabledTools(ctx context.Context, agent *Agent) ([]Tool, error) {
	out := make([]Tool, 0, len(agent.Tools))
	for _, t := range agent.Tools {
		if ft, ok := t.(*FunctionTool); ok {
			// A tool built from an unusable schema/argument type is never sent to
			// the model — fail the run now with a *UserError instead of letting
			// the model call it and receive a schema error (Python raises at
			// decoration time; Go defers construction errors to keep constructors
			// single-valued, so the runner surfaces them here).
			if ft.constructionErr != nil {
				return nil, newUserError("tool %q: %v", ft.Name, ft.constructionErr)
			}
			if ft.IsEnabled != nil {
				ok, err := ft.IsEnabled(ctx, r.rc, agent)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
		}
		out = append(out, t)
	}
	for _, server := range agent.MCPServers {
		mcpTools, err := server.ListTools(ctx, r.rc, agent)
		if err != nil {
			slog.WarnContext(ctx, "MCP ListTools failed, skipping server",
				"agent", agent.Name, "error", err)
			continue
		}
		out = append(out, mcpTools...)
	}
	// Reject duplicate tool names instead of silently letting the last one
	// shadow the others in the runner's dispatch map (the Python SDK raises a
	// UserError for duplicates too).
	seen := make(map[string]bool, len(out))
	for _, t := range out {
		ft, ok := t.(*FunctionTool)
		if !ok {
			continue
		}
		if seen[ft.Name] {
			return nil, newUserError("duplicate tool name %q on agent %q: tool names must be unique across Agent.Tools and MCP server tools", ft.Name, agent.Name)
		}
		seen[ft.Name] = true
	}
	return out, nil
}

// enabledHandoffs returns the agent's handoffs, filtered by any IsEnabled
// predicate (nil means enabled; a predicate error aborts the run). A disabled
// handoff is not offered to the model and cannot be invoked.
func (r *runner) enabledHandoffs(ctx context.Context, agent *Agent) ([]Handoff, error) {
	out := make([]Handoff, 0, len(agent.Handoffs))
	for _, h := range agent.Handoffs {
		if h.IsEnabled != nil {
			ok, err := h.IsEnabled(ctx, r.rc, agent)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, h)
	}
	return out, nil
}

func agentOutputSchema(agent *Agent) OutputSchema {
	if agent.OutputType != nil {
		return agent.OutputType
	}
	return PlainTextOutput()
}

// buildModelInput assembles the model input for a turn: the original input
// followed by every generated item converted back to input form.
func buildModelInput(originalInput []TResponseInputItem, generated []RunItem) ([]TResponseInputItem, error) {
	genInput, err := itemsToInputList(generated)
	if err != nil {
		return nil, err
	}
	out := make([]TResponseInputItem, 0, len(originalInput)+len(genInput))
	out = append(out, originalInput...)
	out = append(out, genInput...)
	return out, nil
}

// normalizeInput coerces a string or []TResponseInputItem into the input list.
func normalizeInput(input any) ([]TResponseInputItem, error) {
	switch v := input.(type) {
	case string:
		return InputItemsFromText(v), nil
	case []TResponseInputItem:
		return v, nil
	case nil:
		return nil, newUserError("run input must not be nil")
	default:
		return nil, newUserError("unsupported run input type %T (want string or []TResponseInputItem)", input)
	}
}

// asAgentsError finds the embedded AgentsError of any SDK error type in err's
// chain (unwrapping fmt.Errorf %w wrapping), so RunErrorDetails can be attached.
func asAgentsError(err error, target **AgentsError) bool {
	if c, ok := errors.AsType[agentsErrorCarrier](err); ok {
		*target = c.base()
		return true
	}
	return false
}
