package agents

import (
	"context"

	"github.com/zzir/agents-go/tracing"
)

// DefaultMaxTurns is the turn budget applied when RunOptions.MaxTurns is zero.
const DefaultMaxTurns = 10

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

	// Hooks receives run-scoped lifecycle callbacks.
	Hooks RunHooks

	// Session, when set, supplies and persists conversation history: prior items
	// are prepended to the input, and the new input plus generated items are
	// saved after the run completes.
	Session Session

	// Tracer, when set, records a trace of the run with a span per model call.
	// Build one with tracing.NewTracer(processor).
	Tracer *tracing.Tracer

	// UsePreviousResponseID opts into server-managed conversation state: instead
	// of resending the full history each turn, the runner chains calls via the
	// OpenAI Responses API's previous_response_id and sends only new items. This
	// saves tokens but requires a model that returns response IDs and keeps
	// responses stored (the default; do not set ModelSettings.Store=false).
	UsePreviousResponseID bool
}

// Run executes the agent loop until the agent produces a final output, hands off
// to another agent that finishes, or the turn budget is exhausted. input may be
// a string or a []TResponseInputItem; use InputItemsFromText for the common
// single-message case.
//
// It is the Go counterpart of the Python SDK's Runner.run.
func Run(ctx context.Context, agent *Agent, input any, opts RunOptions) (*RunResult, error) {
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
		return nil, err
	}

	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, userInput: userInput}

	if opts.Tracer != nil {
		workflow := agent.Name
		if workflow == "" {
			workflow = "Agent workflow"
		}
		r.trace = opts.Tracer.StartTrace(workflow)
		defer r.trace.Finish()
	}

	// With a session, prepend stored history to the model input.
	modelInput := userInput
	if opts.Session != nil {
		history, herr := opts.Session.GetItems(ctx, 0)
		if herr != nil {
			return nil, herr
		}
		if len(history) > 0 {
			modelInput = make([]TResponseInputItem, 0, len(history)+len(userInput))
			modelInput = append(modelInput, history...)
			modelInput = append(modelInput, userInput...)
		}
	}

	return r.loop(ctx, agent, modelInput)
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

	// sessionItems accumulates every generated item for session persistence.
	// Unlike the loop's generatedItems it is never reset by a handoff input
	// filter, so the session keeps the full conversation.
	sessionItems []RunItem

	// toolsUsedBy tracks which agents have called tools this run, driving the
	// tool_choice reset (Agent.DisableToolChoiceReset).
	toolsUsedBy map[*Agent]bool
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

	for turn := startTurn; ; turn++ {
		if turn > r.maxTurns {
			return nil, r.fail(newMaxTurnsError(r.maxTurns), originalInput, generatedItems, rawResponses, currentAgent)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if shouldRunStartHooks {
			if err := callAgentStart(ctx, r.opts.Hooks, currentAgent, r.rc); err != nil {
				return nil, err
			}
			shouldRunStartHooks = false
			// Start an agent span (parent of this agent's generation/tool spans).
			if r.agentSpan != nil {
				r.agentSpan.Finish()
			}
			r.agentSpan = r.trace.StartSpan("agent:"+currentAgent.Name, "")
		}

		model, err := r.resolveModel(currentAgent)
		if err != nil {
			return nil, err
		}
		systemPrompt, err := currentAgent.GetSystemPrompt(ctx, r.rc)
		if err != nil {
			return nil, err
		}
		outputSchema := agentOutputSchema(currentAgent)
		if err := outputSchemaError(outputSchema); err != nil {
			return nil, err
		}
		handoffs := currentAgent.Handoffs
		tools, err := r.enabledTools(ctx, currentAgent)
		if err != nil {
			return nil, err
		}

		// Build the model input. In previous_response_id mode, send only the
		// items the server does not yet have; otherwise send the full history.
		var modelInput []TResponseInputItem
		var prevID string
		if r.opts.UsePreviousResponseID && previousResponseID != "" {
			modelInput, err = itemsToInputList(generatedItems[serverItemCount:])
			prevID = previousResponseID
		} else {
			modelInput, err = buildModelInput(originalInput, generatedItems)
		}
		if err != nil {
			return nil, err
		}

		// On the first turn, run input guardrails concurrently with the model
		// call (matching the Python SDK). guardErrCh delivers a tripwire error.
		// They already ran before an interruption, so a resumed run skips them.
		var guardErrCh chan error
		if turn == startTurn && r.resume == nil && len(startAgent.InputGuardrails) > 0 {
			guardErrCh = make(chan error, 1)
			parentID := r.agentParentID() // read before the goroutine races a handoff
			go func() {
				gspan := r.trace.StartSpan("guardrail:input", parentID)
				_, gerr := runInputGuardrails(ctx, r.rc, startAgent, startAgent.InputGuardrails, originalInput)
				if gerr != nil {
					gspan.SetError(gerr.Error(), nil)
				}
				gspan.Finish()
				guardErrCh <- gerr
			}()
		}

		var resp *ModelResponse
		resumedTurn := pendingResponse != nil
		if resumedTurn {
			// Resuming: re-process the interrupted response (already counted in
			// usage and rawResponses) instead of calling the model again.
			resp = pendingResponse
			pendingResponse = nil
		} else {
			span := r.trace.StartSpan("generation:"+currentAgent.Name, r.agentParentID())
			resp, err = model.GetResponse(ctx, ModelRequest{
				SystemInstructions: systemPrompt,
				Input:              modelInput,
				Settings:           r.resolveSettings(currentAgent),
				Tools:              tools,
				OutputSchema:       outputSchema,
				Handoffs:           handoffs,
				Tracing:            ModelTracingDisabled,
				PreviousResponseID: prevID,
			})
			if err != nil {
				span.SetError(err.Error(), nil)
				span.Finish()
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			span.Set("response_id", resp.ResponseID)
			span.Finish()
			if guardErrCh != nil {
				if gerr := <-guardErrCh; gerr != nil {
					return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
			r.rc.Usage.Add(resp.Usage)
			rawResponses = append(rawResponses, resp)
		}

		processed, err := processModelResponse(currentAgent, tools, handoffs, resp)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		lenBeforeStep := len(generatedItems)
		step, err := r.executeToolsAndSideEffects(ctx, currentAgent, processed, outputSchema, resumedTurn)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
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

		switch step.NextStep {
		case stepFinalOutput:
			if len(currentAgent.OutputGuardrails) > 0 {
				gspan := r.trace.StartSpan("guardrail:output", r.agentParentID())
				_, gerr := runOutputGuardrails(ctx, r.rc, currentAgent, currentAgent.OutputGuardrails, step.FinalOutput)
				if gerr != nil {
					gspan.SetError(gerr.Error(), nil)
				}
				gspan.Finish()
				if gerr != nil {
					return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
			if err := r.saveToSession(ctx); err != nil {
				return nil, err
			}
			if err := callAgentEnd(ctx, r.opts.Hooks, currentAgent, r.rc, step.FinalOutput); err != nil {
				return nil, err
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
				return nil, err
			}
			if step.Handoff != nil && step.Handoff.InputFilter != nil {
				filtered, ferr := applyHandoffInputFilter(step.Handoff, originalInput, generatedItems)
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				originalInput = filtered
				generatedItems = nil
				// The server's stored history is unfiltered, so we can no longer
				// chain via previous_response_id; resend the filtered input.
				previousResponseID = ""
				serverItemCount = 0
			}
			currentAgent = step.NewAgent
			shouldRunStartHooks = true
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
	return r.opts.Session.AddItems(ctx, toSave)
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

func asAgentsError(err error, target **AgentsError) bool {
	switch e := err.(type) {
	case *MaxTurnsError:
		*target = &e.AgentsError
		return true
	case *ModelBehaviorError:
		*target = &e.AgentsError
		return true
	case *ModelRefusalError:
		*target = &e.AgentsError
		return true
	case *UserError:
		*target = &e.AgentsError
		return true
	case *ToolTimeoutError:
		*target = &e.AgentsError
		return true
	case *InputGuardrailTripwireError:
		*target = &e.AgentsError
		return true
	case *OutputGuardrailTripwireError:
		*target = &e.AgentsError
		return true
	case *ToolGuardrailTripwireError:
		*target = &e.AgentsError
		return true
	case *AgentsError:
		*target = e
		return true
	}
	return false
}
