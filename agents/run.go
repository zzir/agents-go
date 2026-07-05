package agents

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/zzir/agents-go/tracing"
)

// DefaultMaxTurns is the turn budget applied when RunOptions.MaxTurns is zero.
const DefaultMaxTurns = 10

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

	// HandoffInputFilter is a run-level default applied to any handoff that does
	// not set its own Handoff.InputFilter. Use NestHandoffHistory to fold prior
	// history across all handoffs.
	HandoffInputFilter func(HandoffInputData) HandoffInputData

	// Hooks receives run-scoped lifecycle callbacks.
	Hooks RunHooks

	// Session, when set, supplies and persists conversation history: prior items
	// are prepended to the input, and the new input plus generated items are
	// saved after the run completes.
	Session Session

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

// Run executes the agent loop until the agent produces a final output, hands off
// to another agent that finishes, or the turn budget is exhausted. input may be
// a string or a []TResponseInputItem; use InputItemsFromText for the common
// single-message case.
//
// It is the Go counterpart of the Python SDK's Runner.run.
func Run(ctx context.Context, agent *Agent, input any, opts RunOptions) (*RunResult, error) {
	r, modelInput, finishTrace, err := prepareRun(ctx, agent, input, opts)
	if err != nil {
		return nil, err
	}
	defer finishTrace()
	return r.loop(ctx, agent, modelInput)
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
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

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

	// With a session, prepend stored history to the model input.
	modelInput := userInput
	if opts.Session != nil {
		history, herr := opts.Session.GetItems(ctx, 0)
		if herr != nil {
			return nil, nil, nil, herr
		}
		if len(history) > 0 {
			modelInput = make([]TResponseInputItem, 0, len(history)+len(userInput))
			modelInput = append(modelInput, history...)
			modelInput = append(modelInput, userInput...)
		}
	}

	return r, modelInput, finishTrace, nil
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

	// sr, when non-nil, makes loop run in streaming mode: raw model events
	// and run-item/agent-updated events are emitted to it, the model is
	// called via StreamResponse, and input guardrails run synchronously
	// before the first model call instead of concurrently with it (the
	// documented difference from blocking runs). These are the ONLY
	// behavioral differences between Run and RunStreamed — everything else
	// lives once in loop.
	sr *StreamedResult

	// sessionItems accumulates every generated item for session persistence.
	// Unlike the loop's generatedItems it is never reset by a handoff input
	// filter, so the session keeps the full conversation.
	sessionItems []RunItem

	// toolsUsedBy tracks which agents have called tools this run, driving the
	// tool_choice reset (Agent.DisableToolChoiceReset).
	toolsUsedBy map[*Agent]bool

	// lastResponseID / lastStore record the final model call's response id and
	// store setting, used to drive session compaction after persistence.
	lastResponseID string
	lastStore      *bool
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

	for turn := startTurn; ; turn++ {
		if turn > r.maxTurns {
			return nil, r.fail(newMaxTurnsError(r.maxTurns), originalInput, generatedItems, rawResponses, currentAgent)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

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

		// On the first turn, run input guardrails: concurrently with the model
		// call for blocking runs (matching the Python SDK), synchronously
		// before it for streaming runs (the documented difference). guardErrCh
		// delivers the concurrent tripwire error. They already ran before an
		// interruption, so a resumed run skips them. The goroutine gets its own
		// cancelable context so early exits (see the deferred
		// cancelInputGuardrails) stop a still-running guardrail.
		var guardErrCh chan error
		if turn == startTurn && r.resume == nil && len(startAgent.InputGuardrails) > 0 {
			if r.sr != nil {
				gspan := r.trace.StartGuardrailSpan("input", r.agentParentID())
				gerr := runInputGuardrails(ctx, r.rc, startAgent, startAgent.InputGuardrails, originalInput)
				if gerr != nil {
					gspan.SetError(gerr.Error(), nil)
				}
				gspan.Finish()
				if gerr != nil {
					return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
			} else {
				guardErrCh = make(chan error, 1)
				gctx, gcancel := context.WithCancel(ctx)
				cancelInputGuardrails = gcancel
				parentID := r.agentParentID() // read before the goroutine races a handoff
				go func() {
					gspan := r.trace.StartGuardrailSpan("input", parentID)
					gerr := runInputGuardrails(gctx, r.rc, startAgent, startAgent.InputGuardrails, originalInput)
					if gerr != nil {
						gspan.SetError(gerr.Error(), nil)
					}
					gspan.Finish()
					guardErrCh <- gerr
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
			if r.opts.CallModelInputFilter != nil {
				edited, ferr := r.opts.CallModelInputFilter(ctx, r.rc, currentAgent, ModelInputData{Instructions: systemPrompt, Input: modelInput})
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				systemPrompt, modelInput = edited.Instructions, edited.Input
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
			if r.sr != nil {
				resp, err = r.streamOneModelCall(ctx, r.sr, span, model, req)
			} else {
				resp, err = model.GetResponse(ctx, req)
			}
			if err != nil {
				span.SetError(err.Error(), nil)
				span.Finish()
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			r.finishGenerationSpan(span, resp)
			// Usage and raw responses must reflect this call even when the
			// OnLLMEnd hook or an input guardrail below aborts the turn
			// (Python parity: the model call already happened and was billed).
			r.rc.Usage.Add(resp.Usage)
			rawResponses = append(rawResponses, resp)
			if err := callLLMEnd(ctx, r.opts.Hooks, currentAgent, r.rc, resp); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if guardErrCh != nil {
				if gerr := <-guardErrCh; gerr != nil {
					return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
		}
		r.lastResponseID = resp.ResponseID
		r.lastStore = r.resolveSettings(currentAgent).Store

		processed, err := processModelResponse(currentAgent, tools, handoffs, resp, r.opts.ToolNotFoundBehavior)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		// Streaming: emit events for the model-produced items.
		if r.sr != nil {
			for _, it := range processed.NewItems {
				r.sr.emit(ctx, &RunItemStreamEvent{Name: runItemEventName(it), Item: it})
			}
		}

		lenBeforeStep := len(generatedItems)
		step, err := r.executeToolsAndSideEffects(ctx, currentAgent, processed, outputSchema, resumedTurn)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		// Streaming: emit events for items produced by side effects
		// (tool/handoff outputs). Safe to slice: streaming never resumes, so
		// NewStepItems always begins with processed.NewItems.
		if r.sr != nil {
			for _, it := range step.NewStepItems[len(processed.NewItems):] {
				r.sr.emit(ctx, &RunItemStreamEvent{Name: runItemEventName(it), Item: it})
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
			if len(currentAgent.OutputGuardrails) > 0 {
				gspan := r.trace.StartGuardrailSpan("output", r.agentParentID())
				gerr := runOutputGuardrails(ctx, r.rc, currentAgent, currentAgent.OutputGuardrails, step.FinalOutput)
				if gerr != nil {
					gspan.SetError(gerr.Error(), nil)
				}
				gspan.Finish()
				if gerr != nil {
					return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
			if err := r.saveToSession(ctx); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if err := callAgentEnd(ctx, r.opts.Hooks, currentAgent, r.rc, step.FinalOutput); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			return &RunResult{
				Input:        originalInput,
				NewItems:     generatedItems,
				RawResponses: rawResponses,
				FinalOutput:  step.FinalOutput,
				LastAgent:    currentAgent,
				Usage:        r.rc.Usage,
			}, nil
		case stepHandoff:
			if err := callHandoff(ctx, r.opts.Hooks, currentAgent, step.NewAgent, r.rc); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if step.Handoff != nil {
				if filter := r.handoffInputFilter(step.Handoff); filter != nil {
					filtered, ferr := applyHandoffInputFilter(filter, originalInput, generatedItems)
					if ferr != nil {
						return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
					}
					originalInput = filtered
					generatedItems = nil
					// The server's stored history is unfiltered, so we can no longer
					// chain via previous_response_id or send conversation deltas;
					// resend the filtered input in full on the next turn.
					previousResponseID = ""
					serverItemCount = 0
					serverCursorActive = false
				}
			}
			currentAgent = step.NewAgent
			shouldRunStartHooks = true
			if r.sr != nil {
				r.sr.emit(ctx, &AgentUpdatedStreamEvent{NewAgent: currentAgent})
			}
			continue
		case stepInterruption:
			state := &RunState{
				CurrentAgent:        currentAgent,
				OriginalInput:       originalInput,
				GeneratedItems:      generatedItems,
				SessionItems:        r.sessionItems,
				UserInput:           r.userInput,
				RawResponses:        rawResponses,
				InterruptedResponse: resp,
				Interruptions:       step.Interruptions,
				Approvals:           r.rc.Approvals,
				Usage:               r.rc.Usage,
				CurrentTurn:         turn,
				MaxTurns:            r.maxTurns,
			}
			return &RunResult{
				Input:         originalInput,
				NewItems:      generatedItems,
				RawResponses:  rawResponses,
				LastAgent:     currentAgent,
				Usage:         r.rc.Usage,
				Interruptions: step.Interruptions,
				State:         state,
			}, nil
		case stepRunAgain:
			continue
		}
	}
}

func (r *runner) fail(err error, input []TResponseInputItem, items []RunItem, raw []*ModelResponse, last *Agent) error {
	// Mark the current agent span failed so the error is visible in traces;
	// child spans (generation, function) set their own errors at the source.
	r.agentSpan.SetError(err.Error(), nil)
	details := &RunErrorDetails{
		Input:        input,
		NewItems:     items,
		RawResponses: raw,
		LastAgent:    last,
		Usage:        r.rc.Usage,
	}
	var ae *AgentsError
	if asAgentsError(err, &ae) {
		ae.Details = details
		return err
	}
	return err
}

// saveToSession persists the new user input and the run's generated items to
// the session, if one is configured. It saves r.sessionItems — the full item
// log, unaffected by handoff input filters — so the stored conversation never
// loses pre-handoff items.
func (r *runner) saveToSession(ctx context.Context) error {
	if r.opts.Session == nil {
		return nil
	}
	genInput, err := itemsToInputList(r.sessionItems)
	if err != nil {
		return err
	}
	toSave := make([]TResponseInputItem, 0, len(r.userInput)+len(genInput))
	toSave = append(toSave, r.userInput...)
	toSave = append(toSave, genInput...)
	if err := r.opts.Session.AddItems(ctx, toSave); err != nil {
		return err
	}
	// If the session can compact itself (e.g. via responses.compact), give it a
	// chance now that the run's items are persisted. At run end there are no
	// pending tool outputs, so compaction is always safe to attempt here.
	// Compaction is best-effort housekeeping: the items are already saved and
	// the final output produced, so a compaction failure is recorded on the
	// trace instead of turning the successful run into an error.
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
	return nil
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

// markToolsUsed records that agent called tools this run (for tool_choice reset).
func (r *runner) markToolsUsed(agent *Agent) {
	if r.toolsUsedBy == nil {
		r.toolsUsedBy = map[*Agent]bool{}
	}
	r.toolsUsedBy[agent] = true
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
	if !agent.DisableToolChoiceReset && s.ToolChoice != "" && r.toolsUsedBy[agent] {
		s.ToolChoice = ""
	}
	return s
}

// enabledTools returns the agent's tools, filtered by any IsEnabled predicate
// and augmented with the tools exposed by the agent's MCP servers.
func (r *runner) enabledTools(ctx context.Context, agent *Agent) ([]Tool, error) {
	out := make([]Tool, 0, len(agent.Tools))
	for _, t := range agent.Tools {
		if ft, ok := t.(*FunctionTool); ok && ft.IsEnabled != nil {
			ok, err := ft.IsEnabled(ctx, r.rc, agent)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, t)
	}
	for _, server := range agent.MCPServers {
		mcpTools, err := server.ListTools(ctx, r.rc, agent)
		if err != nil {
			return nil, err
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
