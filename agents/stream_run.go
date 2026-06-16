package agents

import (
	"context"

	"github.com/openai/openai-go/v3/responses"
)

// usageFromStreamResponse extracts token usage from a streamed final Response.
func usageFromStreamResponse(resp *responses.Response) *Usage {
	u := resp.Usage
	return &Usage{
		Requests:            1,
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		TotalTokens:         u.TotalTokens,
		InputTokensDetails:  InputTokensDetails{CachedTokens: u.InputTokensDetails.CachedTokens},
		OutputTokensDetails: OutputTokensDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens},
	}
}

// runStreamedLoop is the streaming counterpart of runner.loop. It streams each
// model call's raw events, emits RunItem and AgentUpdated events, and returns
// the completed RunResult.
func runStreamedLoop(ctx context.Context, startAgent *Agent, input any, opts RunOptions, sr *StreamedResult) (*RunResult, error) {
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

	userInput, err := normalizeInput(input)
	if err != nil {
		return nil, err
	}
	if err := validateServerState(opts); err != nil {
		return nil, err
	}
	rc.inheritedOpts = &opts
	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, userInput: userInput}

	if opts.Tracer != nil {
		workflow := startAgent.Name
		if workflow == "" {
			workflow = "Agent workflow"
		}
		r.trace = opts.Tracer.StartTrace(workflow)
		defer r.trace.Finish()
	}
	rc.activeTrace = r.trace
	defer func() { r.agentSpan.Finish() }()

	modelInput := userInput
	if opts.Session != nil {
		history, herr := opts.Session.GetItems(ctx, 0)
		if herr != nil {
			return nil, herr
		}
		if len(history) > 0 {
			modelInput = append(append([]TResponseInputItem{}, history...), userInput...)
		}
	}

	currentAgent := startAgent
	generatedItems := []RunItem{}
	rawResponses := []*ModelResponse{}
	shouldRunStartHooks := true

	// previous_response_id tracking, mirroring the non-streaming loop.
	var previousResponseID string
	var serverItemCount int
	var serverCursorActive bool

	for turn := 1; ; turn++ {
		if turn > maxTurns {
			return nil, r.fail(newMaxTurnsError(maxTurns), modelInput, generatedItems, rawResponses, currentAgent)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if shouldRunStartHooks {
			if err := callAgentStart(ctx, opts.Hooks, currentAgent, rc); err != nil {
				return nil, err
			}
			shouldRunStartHooks = false
			if r.agentSpan != nil {
				r.agentSpan.Finish()
			}
			r.agentSpan = r.trace.StartAgentSpan(currentAgent.Name, "")
		}

		model, err := r.resolveModel(currentAgent)
		if err != nil {
			return nil, err
		}
		systemPrompt, err := currentAgent.GetSystemPrompt(ctx, rc)
		if err != nil {
			return nil, err
		}
		prompt, err := currentAgent.GetPrompt(ctx, rc)
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
		var turnInput []TResponseInputItem
		var prevID string
		switch {
		case r.opts.UsePreviousResponseID && previousResponseID != "":
			turnInput, err = itemsToInputList(generatedItems[serverItemCount:])
			prevID = previousResponseID
		case r.opts.ConversationID != "" && serverCursorActive:
			turnInput, err = itemsToInputList(generatedItems[serverItemCount:])
		default:
			turnInput, err = buildModelInput(modelInput, generatedItems)
		}
		if err != nil {
			return nil, err
		}

		// Input guardrails on the first turn, on the same full input as the
		// non-streaming loop (run synchronously here for simplicity).
		if turn == 1 && len(startAgent.InputGuardrails) > 0 {
			gspan := r.trace.StartGuardrailSpan("input", r.agentParentID())
			gerr := runInputGuardrails(ctx, rc, startAgent, startAgent.InputGuardrails, modelInput)
			if gerr != nil {
				gspan.SetError(gerr.Error(), nil)
			}
			gspan.Finish()
			if gerr != nil {
				return nil, r.fail(gerr, modelInput, generatedItems, rawResponses, currentAgent)
			}
		}

		// Stream the model call, forwarding raw events and capturing the response.
		if r.opts.CallModelInputFilter != nil {
			edited, ferr := r.opts.CallModelInputFilter(ctx, rc, currentAgent, ModelInputData{Instructions: systemPrompt, Input: turnInput})
			if ferr != nil {
				return nil, r.fail(ferr, modelInput, generatedItems, rawResponses, currentAgent)
			}
			systemPrompt, turnInput = edited.Instructions, edited.Input
		}
		if err := callLLMStart(ctx, r.opts.Hooks, currentAgent, rc, systemPrompt, turnInput); err != nil {
			return nil, r.fail(err, modelInput, generatedItems, rawResponses, currentAgent)
		}
		span := r.trace.StartGenerationSpan(currentAgent.Name, r.agentParentID())
		resp, err := r.streamOneModelCall(ctx, sr, model, ModelRequest{
			SystemInstructions: systemPrompt,
			Prompt:             prompt,
			Input:              turnInput,
			Settings:           r.resolveSettings(currentAgent),
			Tools:              tools,
			OutputSchema:       outputSchema,
			Handoffs:           handoffs,
			Tracing:            ModelTracingDisabled,
			PreviousResponseID: prevID,
			ConversationID:     r.opts.ConversationID,
		})
		if err != nil {
			span.SetError(err.Error(), nil)
			span.Finish()
			return nil, r.fail(err, modelInput, generatedItems, rawResponses, currentAgent)
		}
		span.Set("response_id", resp.ResponseID)
		setGenerationUsage(span, resp.Usage)
		span.Finish()
		if err := callLLMEnd(ctx, r.opts.Hooks, currentAgent, rc, resp); err != nil {
			return nil, r.fail(err, modelInput, generatedItems, rawResponses, currentAgent)
		}
		rc.Usage.Add(resp.Usage)
		rawResponses = append(rawResponses, resp)
		r.lastResponseID = resp.ResponseID
		r.lastStore = r.resolveSettings(currentAgent).Store

		processed, err := processModelResponse(currentAgent, tools, handoffs, resp, r.opts.ToolNotFoundBehavior)
		if err != nil {
			return nil, r.fail(err, modelInput, generatedItems, rawResponses, currentAgent)
		}

		// Emit events for the model-produced items.
		for _, it := range processed.NewItems {
			sr.emit(ctx, &RunItemStreamEvent{Name: runItemEventName(it), Item: it})
		}

		lenBeforeStep := len(generatedItems)
		step, err := r.executeToolsAndSideEffects(ctx, currentAgent, processed, outputSchema, false)
		if err != nil {
			return nil, r.fail(err, modelInput, generatedItems, rawResponses, currentAgent)
		}

		// Emit events for items produced by side effects (tool/handoff outputs).
		for _, it := range step.NewStepItems[len(processed.NewItems):] {
			sr.emit(ctx, &RunItemStreamEvent{Name: runItemEventName(it), Item: it})
		}

		generatedItems = append(generatedItems, step.NewStepItems...)
		r.sessionItems = append(r.sessionItems, step.NewStepItems...)
		if len(processed.ToolsUsed) > 0 {
			r.markToolsUsed(currentAgent)
		}

		// Advance the server cursor: items already on the server are everything
		// sent this turn plus the model's own output; synthesized items pend.
		if r.opts.UsePreviousResponseID && resp.ResponseID != "" {
			previousResponseID = resp.ResponseID
			serverItemCount = lenBeforeStep + len(processed.NewItems)
		}
		if r.opts.ConversationID != "" {
			serverCursorActive = true
			serverItemCount = lenBeforeStep + len(processed.NewItems)
		}

		switch step.NextStep {
		case stepFinalOutput:
			if gerr := runOutputGuardrails(ctx, rc, currentAgent, currentAgent.OutputGuardrails, step.FinalOutput); gerr != nil {
				return nil, r.fail(gerr, modelInput, generatedItems, rawResponses, currentAgent)
			}
			if err := r.saveToSession(ctx); err != nil {
				return nil, err
			}
			if err := callAgentEnd(ctx, opts.Hooks, currentAgent, rc, step.FinalOutput); err != nil {
				return nil, err
			}
			return &RunResult{
				Input:        modelInput,
				NewItems:     generatedItems,
				RawResponses: rawResponses,
				FinalOutput:  step.FinalOutput,
				LastAgent:    currentAgent,
				Usage:        rc.Usage,
			}, nil
		case stepHandoff:
			if err := callHandoff(ctx, opts.Hooks, currentAgent, step.NewAgent, rc); err != nil {
				return nil, err
			}
			if step.Handoff != nil {
				if filter := r.handoffInputFilter(step.Handoff); filter != nil {
					filtered, ferr := applyHandoffInputFilter(filter, modelInput, generatedItems)
					if ferr != nil {
						return nil, r.fail(ferr, modelInput, generatedItems, rawResponses, currentAgent)
					}
					modelInput = filtered
					generatedItems = nil
					// Filtered history can't chain via previous_response_id.
					previousResponseID = ""
					serverItemCount = 0
				}
			}
			currentAgent = step.NewAgent
			shouldRunStartHooks = true
			sr.emit(ctx, &AgentUpdatedStreamEvent{NewAgent: currentAgent})
			continue
		case stepInterruption:
			state := &RunState{
				CurrentAgent:        currentAgent,
				OriginalInput:       modelInput,
				GeneratedItems:      generatedItems,
				SessionItems:        r.sessionItems,
				UserInput:           userInput,
				RawResponses:        rawResponses,
				InterruptedResponse: resp,
				Interruptions:       step.Interruptions,
				Approvals:           rc.Approvals,
				Usage:               rc.Usage,
				CurrentTurn:         turn,
			}
			return &RunResult{
				Input:         modelInput,
				NewItems:      generatedItems,
				RawResponses:  rawResponses,
				LastAgent:     currentAgent,
				Usage:         rc.Usage,
				Interruptions: step.Interruptions,
				State:         state,
			}, nil
		case stepRunAgain:
			continue
		}
	}
}

// streamOneModelCall streams a single model call, forwarding each raw event to
// the consumer and assembling the final ModelResponse from the completed event.
func (r *runner) streamOneModelCall(ctx context.Context, sr *StreamedResult, model Model, req ModelRequest) (*ModelResponse, error) {
	var final *ModelResponse
	for event, err := range model.StreamResponse(ctx, req) {
		if err != nil {
			return nil, err
		}
		sr.emit(ctx, &RawResponsesStreamEvent{Data: event})
		if event != nil && event.Type == "response.completed" {
			completed := event.AsResponseCompleted()
			final = &ModelResponse{
				Output:     completed.Response.Output,
				Usage:      usageFromStreamResponse(&completed.Response),
				ResponseID: completed.Response.ID,
			}
		}
	}
	if final == nil {
		// No response.completed event arrived: the stream ended early or with a
		// terminal failure event. Surfacing this is essential — fabricating an
		// empty response would make a failed run "succeed" with empty output.
		return nil, newModelBehaviorError("model stream ended without a completed response")
	}
	return final, nil
}
