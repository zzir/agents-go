package agents

import (
	"context"
	"fmt"
	"slices"

	"golang.org/x/sync/errgroup"
)

// nextStepKind enumerates what the runner should do after a single turn.
type nextStepKind int

const (
	stepRunAgain nextStepKind = iota
	stepFinalOutput
	stepHandoff
	stepInterruption
)

// singleStepResult is the outcome of processing one model response and running
// any resulting tools/handoffs.
type singleStepResult struct {
	NewStepItems  []RunItem
	NextStep      nextStepKind
	FinalOutput   any
	NewAgent      *Agent
	Handoff       *Handoff // the handoff taken, when NextStep is stepHandoff
	Interruptions []*ToolApprovalItem
}

// toolRunFunction pairs a function tool call with the tool that handles it.
type toolRunFunction struct {
	Tool *FunctionTool
	Call functionCall
}

// toolRunHandoff pairs a handoff tool call with the handoff that handles it.
type toolRunHandoff struct {
	Handoff Handoff
	Call    functionCall
}

// functionCall is the minimal view of a model-emitted function/handoff call.
type functionCall struct {
	CallID    string
	Name      string
	Arguments string
	Raw       TResponseOutputItem
}

// ToolNotFoundBehavior controls handling of a model tool call that names no
// known tool on the agent.
type ToolNotFoundBehavior int

const (
	// ToolNotFoundError aborts the run with a ModelBehaviorError (default).
	ToolNotFoundError ToolNotFoundBehavior = iota
	// ToolNotFoundReturnToModel synthesizes an error tool output and continues,
	// letting the model correct itself on the next turn.
	ToolNotFoundReturnToModel
)

// processedResponse is the classified content of a model response.
type processedResponse struct {
	NewItems     []RunItem
	Functions    []toolRunFunction
	Handoffs     []toolRunHandoff
	ToolsUsed    []string
	UnknownTools []functionCall
}

func (p *processedResponse) hasToolsToRun() bool {
	return len(p.Functions) > 0 || len(p.Handoffs) > 0 || len(p.UnknownTools) > 0
}

// processModelResponse classifies each output item of a model response into run
// items and pending tool/handoff actions. It is the Go counterpart of the Python
// SDK's process_model_response, covering messages, reasoning, function calls and
// handoff calls.
func processModelResponse(
	agent *Agent,
	tools []Tool,
	handoffs []Handoff,
	resp *ModelResponse,
	toolNotFound ToolNotFoundBehavior,
) (*processedResponse, error) {
	handoffMap := make(map[string]Handoff, len(handoffs))
	for _, h := range handoffs {
		handoffMap[h.ToolName] = h
	}
	functionMap := make(map[string]*FunctionTool)
	for _, t := range tools {
		if ft, ok := t.(*FunctionTool); ok {
			functionMap[ft.Name] = ft
		}
	}

	pr := &processedResponse{}
	for _, output := range resp.Output {
		switch output.Type {
		case "message":
			pr.NewItems = append(pr.NewItems, &MessageOutputItem{Agent: agent, Raw: output})
		case "reasoning":
			pr.NewItems = append(pr.NewItems, &ReasoningItem{Agent: agent, Raw: output})
		case "function_call":
			fc := output.AsFunctionCall()
			call := functionCall{CallID: fc.CallID, Name: fc.Name, Arguments: fc.Arguments, Raw: output}
			pr.ToolsUsed = append(pr.ToolsUsed, fc.Name)
			if h, ok := handoffMap[fc.Name]; ok {
				pr.NewItems = append(pr.NewItems, &HandoffCallItem{Agent: agent, Raw: output})
				pr.Handoffs = append(pr.Handoffs, toolRunHandoff{Handoff: h, Call: call})
				continue
			}
			ft, ok := functionMap[fc.Name]
			if !ok {
				if toolNotFound == ToolNotFoundReturnToModel {
					// Record the call and let executeToolsAndSideEffects synthesize
					// an error output, so the model can correct itself next turn.
					pr.NewItems = append(pr.NewItems, &ToolCallItem{Agent: agent, Raw: output})
					pr.UnknownTools = append(pr.UnknownTools, call)
					continue
				}
				return nil, newModelBehaviorError("tool %q not found on agent %q", fc.Name, agent.Name)
			}
			pr.NewItems = append(pr.NewItems, &ToolCallItem{Agent: agent, Raw: output})
			pr.Functions = append(pr.Functions, toolRunFunction{Tool: ft, Call: call})
		default:
			// The supported tool set produces only the item types handled above;
			// ignore any other output item type defensively.
		}
	}
	return pr, nil
}

// executeToolsAndSideEffects runs the tools and handoffs requested by a model
// response and determines the next step. It mirrors the decision logic of the
// Python SDK's execute_tools_and_side_effects / get_single_step_result.
//
// resumed marks the first turn after a HITL resume: the interrupted response's
// own items were already recorded before the run paused, so they must not be
// appended a second time.
func (r *runner) executeToolsAndSideEffects(
	ctx context.Context,
	agent *Agent,
	pr *processedResponse,
	outputSchema OutputSchema,
	resumed bool,
) (*singleStepResult, error) {
	newStepItems := make([]RunItem, 0, len(pr.NewItems))
	if !resumed {
		newStepItems = append(newStepItems, pr.NewItems...)
	}

	// Human-in-the-loop: partition function calls into those ready to run, those
	// awaiting approval, and synthetic outputs for rejected calls.
	toRun, interruptions, rejectedItems, err := r.partitionByApproval(ctx, agent, pr.Functions)
	if err != nil {
		return nil, err
	}

	// If any tool call awaits approval, pause the run before executing anything
	// so that nothing runs twice when the run resumes.
	if len(interruptions) > 0 {
		return &singleStepResult{
			NewStepItems:  newStepItems,
			NextStep:      stepInterruption,
			Interruptions: interruptions,
		}, nil
	}

	newStepItems = append(newStepItems, rejectedItems...)

	// Run the approved/no-approval-needed function tools in parallel.
	functionResults, err := r.runFunctionTools(ctx, agent, toRun)
	if err != nil {
		return nil, err
	}
	for _, fr := range functionResults {
		newStepItems = append(newStepItems, fr.outputItem)
	}

	// Unknown tool calls (ToolNotFoundReturnToModel): feed an error output back so
	// the model can correct itself. hasToolsToRun stays true, forcing another turn.
	for _, call := range pr.UnknownTools {
		msg := fmt.Sprintf("Error: tool %q not found.", call.Name)
		newStepItems = append(newStepItems, newFunctionCallOutputItem(agent, call.CallID, msg))
	}

	// Handoffs take precedence: switch to the first requested target agent.
	if len(pr.Handoffs) > 0 {
		return r.executeHandoff(ctx, agent, pr.Handoffs, newStepItems)
	}

	// tool_use_behavior: stop using a tool's output as the final result.
	stop, output, err := r.checkToolUseBehavior(ctx, agent, functionResults)
	if err != nil {
		return nil, err
	}
	if stop {
		return &singleStepResult{
			NewStepItems: newStepItems,
			NextStep:     stepFinalOutput,
			FinalOutput:  output,
		}, nil
	}

	// Determine whether the model produced a final output this turn.
	lastMessage := lastMessageItem(newStepItems)
	if !pr.hasToolsToRun() {
		// No tools to run: this is a candidate final output.
		if lastMessage != nil {
			text := lastMessage.Text()
			// A message with no text but a refusal is a refusal, not an empty
			// (or unparsable) final output.
			if text == "" {
				if refusal := extractMessageRefusal(lastMessage.Raw); refusal != "" {
					return nil, &ModelRefusalError{
						AgentsError: AgentsError{Message: "model refused to respond: " + refusal},
						Refusal:     refusal,
					}
				}
			}
			if outputSchema != nil && !outputSchema.IsPlainText() {
				final, err := outputSchema.ValidateJSON(text)
				if err != nil {
					return nil, newModelBehaviorError("failed to parse structured output: %v", err)
				}
				return &singleStepResult{NewStepItems: newStepItems, NextStep: stepFinalOutput, FinalOutput: final}, nil
			}
			return &singleStepResult{NewStepItems: newStepItems, NextStep: stepFinalOutput, FinalOutput: text}, nil
		}
		// No message and no tools: nothing actionable. Treat empty string as
		// final output for plain-text agents to avoid an infinite loop.
		if outputSchema == nil || outputSchema.IsPlainText() {
			if len(pr.ToolsUsed) == 0 {
				return &singleStepResult{NewStepItems: newStepItems, NextStep: stepFinalOutput, FinalOutput: ""}, nil
			}
		}
	}

	// Otherwise, feed tool results back to the model for another turn.
	return &singleStepResult{NewStepItems: newStepItems, NextStep: stepRunAgain}, nil
}

// functionToolResult bundles a tool's output item with the tool and raw value.
type functionToolResult struct {
	tool       *FunctionTool
	outputItem *ToolCallOutputItem
	output     any
}

// runFunctionTools invokes every function tool call concurrently, returning
// results in the original call order. Hook callbacks fire around each call.
func (r *runner) runFunctionTools(ctx context.Context, agent *Agent, runs []toolRunFunction) ([]functionToolResult, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	results := make([]functionToolResult, len(runs))
	g, gctx := errgroup.WithContext(ctx)
	if r.opts.MaxToolConcurrency > 0 {
		g.SetLimit(r.opts.MaxToolConcurrency)
	}
	for i, run := range runs {
		g.Go(func() error {
			tc := &ToolContext{
				RunContext:    r.rc,
				ToolName:      run.Call.Name,
				ToolCallID:    run.Call.CallID,
				ToolArguments: run.Call.Arguments,
			}
			if err := callToolStart(gctx, r.opts.Hooks, agent, r.rc, run.Tool); err != nil {
				return err
			}

			// Tool input guardrails: may reject (substitute content) or raise.
			if rejected, msg, err := r.runToolInputGuardrails(gctx, agent, run); err != nil {
				return err
			} else if rejected {
				results[i] = functionToolResult{
					tool:       run.Tool,
					outputItem: newFunctionCallOutputItem(agent, run.Call.CallID, msg),
					output:     msg,
				}
				return nil
			}

			span := r.trace.StartSpan("function:"+run.Call.Name, r.agentParentID())
			defer span.Finish() // idempotent; covers panics in user tool code
			out, err := invokeTool(gctx, run.Tool, tc, run.Call.Arguments)
			if err != nil {
				span.SetError(err.Error(), nil)
				span.Finish()
				// A FailureErrorFunction feeds the error back to the model as the
				// tool output so it can recover; otherwise the error aborts the run.
				if run.Tool.FailureErrorFunction == nil {
					return fmt.Errorf("tool %q failed: %w", run.Call.Name, err)
				}
				msg := run.Tool.FailureErrorFunction(gctx, tc, err)
				results[i] = functionToolResult{
					tool:       run.Tool,
					outputItem: newFunctionCallOutputItem(agent, run.Call.CallID, msg),
					output:     msg,
				}
				return nil
			}
			span.Finish()

			// Tool output guardrails: may reject (substitute content) or raise.
			if rejected, msg, err := r.runToolOutputGuardrails(gctx, agent, run, out); err != nil {
				return err
			} else if rejected {
				out = msg
			}

			if err := callToolEnd(gctx, r.opts.Hooks, agent, r.rc, run.Tool, out); err != nil {
				return err
			}
			results[i] = functionToolResult{
				tool:       run.Tool,
				outputItem: newFunctionCallOutputItem(agent, run.Call.CallID, out),
				output:     out,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// DefaultRejectionMessage is sent back to the model when a tool call is rejected
// without a custom message.
const DefaultRejectionMessage = "Tool execution was not approved."

// partitionByApproval splits function tool calls into those ready to run, those
// awaiting human approval (interruptions), and synthetic outputs for rejected
// calls. It consults the run context's ApprovalStore.
func (r *runner) partitionByApproval(ctx context.Context, agent *Agent, runs []toolRunFunction) (toRun []toolRunFunction, interruptions []*ToolApprovalItem, rejected []RunItem, err error) {
	store := r.rc.Approvals
	for _, run := range runs {
		needs, aerr := run.Tool.requiresApproval(ctx, r.rc, run.Call.Arguments)
		if aerr != nil {
			return nil, nil, nil, aerr
		}
		if !needs {
			toRun = append(toRun, run)
			continue
		}
		decision, decided := approvalDecision{}, false
		if store != nil {
			decision, decided = store.decisionFor(run.Call.Name, run.Call.CallID)
		}
		switch {
		case !decided:
			interruptions = append(interruptions, &ToolApprovalItem{
				Agent:     agent,
				ToolName:  run.Call.Name,
				CallID:    run.Call.CallID,
				Arguments: run.Call.Arguments,
				Raw:       run.Call.Raw,
			})
		case decision.rejected:
			msg := decision.message
			if msg == "" {
				msg = DefaultRejectionMessage
			}
			rejected = append(rejected, newFunctionCallOutputItem(agent, run.Call.CallID, msg))
		default: // approved
			toRun = append(toRun, run)
		}
	}
	return toRun, interruptions, rejected, nil
}

// runToolInputGuardrails runs a tool's input guardrails. It returns
// (rejected, replacementMessage, error): rejected with a message means the tool
// should be skipped and the message returned to the model; an error halts the run.
func (r *runner) runToolInputGuardrails(ctx context.Context, agent *Agent, run toolRunFunction) (bool, string, error) {
	for _, g := range run.Tool.InputGuardrails {
		out, err := g.Run(ctx, r.rc, ToolInputGuardrailData{
			Agent:      agent,
			ToolName:   run.Call.Name,
			ToolCallID: run.Call.CallID,
			Arguments:  run.Call.Arguments,
		})
		if err != nil {
			return false, "", err
		}
		switch out.Behavior {
		case ToolGuardrailRejectContent:
			return true, out.Message, nil
		case ToolGuardrailRaiseException:
			return false, "", &ToolGuardrailTripwireError{
				AgentsError:   AgentsError{Message: "tool input guardrail " + g.Name + " tripwire triggered"},
				GuardrailName: g.Name, ToolName: run.Call.Name,
			}
		}
	}
	return false, "", nil
}

// runToolOutputGuardrails runs a tool's output guardrails on its result.
func (r *runner) runToolOutputGuardrails(ctx context.Context, agent *Agent, run toolRunFunction, output any) (bool, string, error) {
	for _, g := range run.Tool.OutputGuardrails {
		out, err := g.Run(ctx, r.rc, ToolOutputGuardrailData{
			Agent:      agent,
			ToolName:   run.Call.Name,
			ToolCallID: run.Call.CallID,
			Arguments:  run.Call.Arguments,
			Output:     output,
		})
		if err != nil {
			return false, "", err
		}
		switch out.Behavior {
		case ToolGuardrailRejectContent:
			return true, out.Message, nil
		case ToolGuardrailRaiseException:
			return false, "", &ToolGuardrailTripwireError{
				AgentsError:   AgentsError{Message: "tool output guardrail " + g.Name + " tripwire triggered"},
				GuardrailName: g.Name, ToolName: run.Call.Name,
			}
		}
	}
	return false, "", nil
}

func invokeTool(ctx context.Context, tool *FunctionTool, tc *ToolContext, argsJSON string) (any, error) {
	if tool.OnInvoke == nil {
		return nil, newUserError("function tool %q has no OnInvoke", tool.Name)
	}
	if tool.Timeout <= 0 {
		return tool.OnInvoke(ctx, tc, argsJSON)
	}
	tctx, cancel := context.WithTimeout(ctx, tool.Timeout)
	defer cancel()
	out, err := tool.OnInvoke(tctx, tc, argsJSON)
	// Only the tool's own deadline (not a caller cancellation) is a timeout.
	if err != nil && tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return nil, &ToolTimeoutError{
			AgentsError: AgentsError{Message: fmt.Sprintf("tool %q timed out after %v", tool.Name, tool.Timeout)},
			ToolName:    tool.Name,
		}
	}
	return out, err
}

// multipleHandoffsMessage is sent back as the tool output for every handoff
// beyond the first when the model requests several in one turn. It matches the
// Python SDK's message.
const multipleHandoffsMessage = "Multiple handoffs detected, ignoring this one."

// executeHandoff switches to the first requested handoff target, recording a
// synthetic handoff output item.
func (r *runner) executeHandoff(ctx context.Context, from *Agent, handoffs []toolRunHandoff, newStepItems []RunItem) (*singleStepResult, error) {
	run := handoffs[0]
	// Every handoff call the model emitted is in the conversation as a
	// function_call; the ones we ignore still need an output item, or the next
	// model call is rejected for a dangling call.
	for _, ignored := range handoffs[1:] {
		newStepItems = append(newStepItems, newFunctionCallOutputItem(from, ignored.Call.CallID, multipleHandoffsMessage))
	}
	span := r.trace.StartSpan("handoff:"+run.Handoff.ToolName, r.agentParentID())
	defer span.Finish()
	if run.Handoff.OnInvoke == nil {
		return nil, newUserError("handoff %q has no OnInvoke", run.Handoff.ToolName)
	}
	target, err := run.Handoff.OnInvoke(ctx, r.rc, run.Call.Arguments)
	if err != nil {
		return nil, fmt.Errorf("handoff %q failed: %w", run.Handoff.ToolName, err)
	}
	if target == nil {
		return nil, newModelBehaviorError("handoff %q returned a nil agent", run.Handoff.ToolName)
	}
	if run.Handoff.OnHandoff != nil {
		if err := run.Handoff.OnHandoff(ctx, r.rc, run.Call.Arguments); err != nil {
			return nil, fmt.Errorf("handoff %q on-handoff callback failed: %w", run.Handoff.ToolName, err)
		}
	}

	outputItem := &HandoffOutputItem{
		Agent:       from,
		Raw:         handoffOutputInput(run.Call.CallID, target.Name),
		SourceAgent: from,
		TargetAgent: target,
	}
	newStepItems = append(newStepItems, outputItem)

	h := run.Handoff
	return &singleStepResult{
		NewStepItems: newStepItems,
		NextStep:     stepHandoff,
		NewAgent:     target,
		Handoff:      &h,
	}, nil
}

// checkToolUseBehavior applies an agent's ToolUseBehavior to the function tool
// results. It returns (stop, finalOutput, err); stop indicates the run should end
// with finalOutput.
func (r *runner) checkToolUseBehavior(ctx context.Context, agent *Agent, results []functionToolResult) (bool, any, error) {
	if len(results) == 0 {
		return false, nil, nil
	}
	switch b := agent.ToolUseBehavior.(type) {
	case nil, RunLLMAgain:
		return false, nil, nil
	case StopOnFirstTool:
		return true, results[0].output, nil
	case StopAtTools:
		for _, res := range results {
			if slices.Contains(b.Names, res.tool.Name) {
				return true, res.output, nil
			}
		}
		return false, nil, nil
	case ToolUseBehaviorFunc:
		public := make([]FunctionToolResult, len(results))
		for i, res := range results {
			public[i] = FunctionToolResult{ToolName: res.tool.Name, Output: res.output}
		}
		stop, output, err := b(ctx, r.rc, public)
		return stop, output, err
	default:
		return false, nil, nil
	}
}

// applyHandoffInputFilter builds the full conversation input and runs filter
// over it, returning the filtered input for the next agent.
func applyHandoffInputFilter(filter func(HandoffInputData) HandoffInputData, originalInput []TResponseInputItem, generated []RunItem) ([]TResponseInputItem, error) {
	full, err := buildModelInput(originalInput, generated)
	if err != nil {
		return nil, err
	}
	out := filter(HandoffInputData{InputHistory: full})
	return out.InputHistory, nil
}

// handoffInputFilter resolves the filter for a handoff: the handoff's own
// InputFilter takes precedence over the run-level RunOptions.HandoffInputFilter.
// Returns nil when neither is set.
func (r *runner) handoffInputFilter(h *Handoff) func(HandoffInputData) HandoffInputData {
	if h.InputFilter != nil {
		return h.InputFilter
	}
	return r.opts.HandoffInputFilter
}

func lastMessageItem(items []RunItem) *MessageOutputItem {
	for i := len(items) - 1; i >= 0; i-- {
		if m, ok := items[i].(*MessageOutputItem); ok {
			return m
		}
	}
	return nil
}
