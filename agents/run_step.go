package agents

import (
	"context"
	"fmt"
	"log/slog"
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
	NewStepItems  []*RunItem
	NextStep      nextStepKind
	FinalOutput   any
	NewAgent      *Agent
	Handoff       *Handoff // the handoff taken, when NextStep is stepHandoff
	Interruptions []*ToolApprovalItem
	// NestedStates carries the paused RunState of any agent-as-tool nested run
	// that interrupted this turn, keyed by the parent tool call id, so the
	// runner can stash it on the parent RunState for ResumeRun.
	NestedStates map[string]*RunState
}

// toolRunFunction pairs a function tool call with the tool that handles it.
type toolRunFunction struct {
	Tool *Tool
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
	Raw       OutputItem
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

// ParseToolNotFoundBehavior converts a string to a ToolNotFoundBehavior.
// Recognized values: "error" (or ""), "return_to_model" and its upstream alias
// "return_error_to_model". Unknown values return ToolNotFoundError.
func ParseToolNotFoundBehavior(s string) ToolNotFoundBehavior {
	switch s {
	case "return_to_model", "return_error_to_model":
		return ToolNotFoundReturnToModel
	default:
		return ToolNotFoundError
	}
}

// String returns the string representation of the behavior.
func (b ToolNotFoundBehavior) String() string {
	switch b {
	case ToolNotFoundReturnToModel:
		return "return_to_model"
	default:
		return "error"
	}
}

// processedResponse is the classified content of a model response.
type processedResponse struct {
	NewItems     []*RunItem
	Functions    []toolRunFunction
	Handoffs     []toolRunHandoff
	ToolsUsed    []string
	UnknownTools []functionCall
}

func (p *processedResponse) hasToolsToRun() bool {
	return len(p.Functions) > 0 || len(p.Handoffs) > 0 || len(p.UnknownTools) > 0
}

// processModelResponse classifies each output item of a model response into
// run items and pending tool/handoff actions.
func processModelResponse(
	agent *Agent,
	tools []*Tool,
	handoffs []Handoff,
	resp *ModelResponse,
	toolNotFound ToolNotFoundBehavior,
) (*processedResponse, error) {
	handoffMap := make(map[string]Handoff, len(handoffs))
	for _, h := range handoffs {
		handoffMap[h.ToolName] = h
	}
	// Every tool is dispatchable — one with no OnInvoke fails AT INVOCATION as
	// a UserError naming the tool. Filtering it out here instead would route
	// the model's call to the not-found path, blaming the model for a
	// configuration bug (and under ToolNotFoundReturnToModel, inviting it to
	// retry forever against one).
	functionMap := make(map[string]*Tool)
	for _, t := range tools {
		functionMap[t.Name] = t
	}

	pr := &processedResponse{}
	for _, output := range resp.Output {
		switch output.Type {
		case "message":
			pr.NewItems = append(pr.NewItems, NewModelItem(ItemMessage, agent, output))
		case "reasoning":
			pr.NewItems = append(pr.NewItems, NewModelItem(ItemReasoning, agent, output))
		case "function_call":
			fc := output.AsFunctionCall()
			call := functionCall{CallID: fc.CallID, Name: fc.Name, Arguments: fc.Arguments, Raw: output}
			pr.ToolsUsed = append(pr.ToolsUsed, fc.Name)
			if h, ok := handoffMap[fc.Name]; ok {
				pr.NewItems = append(pr.NewItems, NewModelItem(ItemHandoffCall, agent, output))
				pr.Handoffs = append(pr.Handoffs, toolRunHandoff{Handoff: h, Call: call})
				continue
			}
			ft, ok := functionMap[fc.Name]
			if !ok {
				if toolNotFound == ToolNotFoundReturnToModel {
					// Record the call and let executeToolsAndSideEffects synthesize
					// an error output, so the model can correct itself next turn.
					pr.NewItems = append(pr.NewItems, NewModelItem(ItemToolCall, agent, output))
					pr.UnknownTools = append(pr.UnknownTools, call)
					continue
				}
				return nil, NewModelBehaviorError("tool %q not found on agent %q", fc.Name, agent.Name)
			}
			pr.NewItems = append(pr.NewItems, NewModelItem(ItemToolCall, agent, output))
			pr.Functions = append(pr.Functions, toolRunFunction{Tool: ft, Call: call})
		default:
			// An item type this SDK does not model. Keeping it is not optional:
			// the Responses API gains types faster than any client tracks them,
			// and dropping one corrupts the conversation, because the next turn
			// resends a history the model does not recognize as its own.
			//
			// ItemUnknown carries the bytes through untouched. Such items used
			// to be silently discarded here.
			pr.NewItems = append(pr.NewItems, NewModelItem(ItemUnknown, agent, output))
		}
	}
	return pr, nil
}

// executeToolsAndSideEffects runs the tools and handoffs requested by a model
// response and determines the next step.
//
// resumed marks the first turn after a HITL resume: the interrupted response's
// own items were already recorded before the run paused, so they must not be
// appended a second time. originalInput, preStepItems and resp feed the
// RunErrorData snapshot when an ErrorHandlers recovery fires.
func (r *runner) executeToolsAndSideEffects(
	ctx context.Context,
	agent *Agent,
	pr *processedResponse,
	outputSchema OutputSchema,
	resumed bool,
	originalInput []InputItem,
	preStepItems []*RunItem,
	resp *ModelResponse,
) (*singleStepResult, error) {
	newStepItems := make([]*RunItem, 0, len(pr.NewItems))
	if !resumed {
		newStepItems = append(newStepItems, pr.NewItems...)
	}

	// On the first turn after a HITL resume the interrupted model response is
	// re-processed, so any sibling tool calls that already completed before the
	// pause reappear in pr.Functions. Their outputs were recorded before the
	// pause and still sit in preStepItems, so re-running them would duplicate
	// their side effects and emit a second function_call_output for the same call
	// id — which the Responses API rejects on the next turn. Drop them from this
	// turn's work so they are neither re-run nor re-output (their prior outputs
	// stay in the item log). Mirrors openai-agents-python 3229/3259.
	functions := pr.Functions
	if resumed {
		functions = dropCompletedResumedCalls(functions, preStepItems)
	}

	// Human-in-the-loop: partition function calls into those ready to run, those
	// awaiting approval, and rejected calls (already resolved to results).
	//
	// A truncated response bypasses the partition: a call whose arguments may
	// stop mid-JSON must neither run nor ASK. Pausing would put a doomed call
	// in front of a human — and an approval serialized into a RunState and
	// resumed in another process would then execute what this process refuses,
	// since nothing after the pause re-checks what only the truncation guard
	// below knows. Every call falls through to truncatedCallResults instead.
	var toRun []toolRunFunction
	var interruptions []*ToolApprovalItem
	var rejected []functionToolResult
	var err error
	if resp.Truncated() {
		toRun = functions
	} else {
		toRun, interruptions, rejected, err = r.partitionByApproval(ctx, agent, functions)
		if err != nil {
			return nil, err
		}
	}

	// If any tool call awaits approval, pause the run before executing anything
	// so that nothing runs twice when the run resumes.
	if len(interruptions) > 0 {
		r.log.component("tool").Info(ctx, "waiting for approval",
			slog.Int("pending", len(interruptions)))
		return &singleStepResult{
			NewStepItems:  newStepItems,
			NextStep:      stepInterruption,
			Interruptions: interruptions,
		}, nil
	}

	// Run the approved/no-approval-needed function tools in parallel, then merge
	// with the rejected results in original call order so item order and
	// the turn-boundary hooks see every call (results built in tool_runs
	// order).
	var executed []functionToolResult
	if resp.Truncated() {
		// The response was cut off at the output-token limit, so a call's
		// arguments may stop mid-JSON. None of them run.
		r.log.component("tool").Warn(ctx, "response truncated; refusing to run its tool calls",
			slog.Int("calls", len(toRun)))
		RecordDiagnostic(ctx, DiagResponseTruncated, nil, map[string]any{
			"calls": len(toRun), "response_id": resp.ResponseID,
		})
		executed = truncatedCallResults(agent, toRun)
	} else {
		executed, err = r.runFunctionTools(ctx, agent, toRun)
		if err != nil {
			return nil, err
		}
	}
	functionResults := orderToolResults(functions, executed, rejected)

	// A model stuck calling a broken tool would otherwise burn the whole turn
	// budget rediscovering that it is broken, and bill for it.
	if err := r.noteToolTurn(functionResults); err != nil {
		return nil, err
	}
	r.discloseTools(functionResults)

	var nestedInterruptions []*ToolApprovalItem
	var nestedStates map[string]*RunState
	for _, fr := range functionResults {
		if len(fr.nestedInterruptions) > 0 {
			// An agent-as-tool's nested run paused for approval: gather its
			// interruptions and cache its state by call id. Its output is
			// withheld (fr.outputItem is nil) until the parent run resumes.
			nestedInterruptions = append(nestedInterruptions, fr.nestedInterruptions...)
			if fr.nestedState != nil {
				if nestedStates == nil {
					nestedStates = map[string]*RunState{}
				}
				nestedStates[fr.callID] = fr.nestedState
			}
			continue
		}
		if fr.outputItem != nil {
			newStepItems = append(newStepItems, fr.outputItem)
		}
	}

	// If any nested agent-as-tool run paused, pause the parent run too: surface
	// the nested interruptions as the parent's own and carry the paused nested
	// states so ResumeRun continues them. Sibling tools that completed keep their
	// outputs in newStepItems.
	if len(nestedInterruptions) > 0 {
		return &singleStepResult{
			NewStepItems:  newStepItems,
			NextStep:      stepInterruption,
			Interruptions: nestedInterruptions,
			NestedStates:  nestedStates,
		}, nil
	}

	// Unknown tool calls (ToolNotFoundReturnToModel): feed an error output back so
	// the model can correct itself. hasToolsToRun stays true, forcing another turn.
	for _, call := range pr.UnknownTools {
		// Attach the tool-not-found error to the current agent span. The tool name
		// is model-chosen metadata, not user data, so it is recorded regardless of
		// the sensitive-data setting.
		r.agentSpan.SetError("Tool not found", map[string]any{"tool_name": call.Name})
		msg := fmt.Sprintf("Tool '%s' not found.", call.Name)
		newStepItems = append(newStepItems, newFunctionCallOutputItem(agent, call.CallID, msg))
	}

	// Handoffs take precedence: switch to the first requested target agent.
	if len(pr.Handoffs) > 0 {
		return r.executeHandoff(ctx, agent, pr.Handoffs, newStepItems)
	}

	// Every tool in the batch asked to stop: honor it, using the last output as
	// the final result.
	//
	// Unanimity is the rule, not "any". One tool wanting to stop while another
	// is still working is not a decision the SDK can make for them, and
	// stopping anyway would throw away the other's result — which the model
	// asked for and the user paid for.
	if allTerminate(functionResults) {
		return &singleStepResult{
			NewStepItems: newStepItems,
			NextStep:     stepFinalOutput,
			FinalOutput:  coerceToolFinalOutput(agent, functionResults[len(functionResults)-1].output),
		}, nil
	}

	// Determine whether the model produced a final output this turn.
	lastMessage := lastMessageItem(newStepItems)
	if !pr.hasToolsToRun() {
		// Tool activity without any message (e.g. only rejected calls): the
		// results must go back to the model.
		hasToolActivityWithoutMessage := lastMessage == nil && len(pr.ToolsUsed) > 0
		if !hasToolActivityWithoutMessage {
			var text string
			if lastMessage != nil {
				text = lastMessage.Text()
				// A refusal fails the run (recoverable via
				// ErrorHandlers.ModelRefusal), taking precedence over any text
				// or structured content in the same message.
				if refusal := lastMessage.refusal(); refusal != "" {
					refErr := &ModelRefusalError{Refusal: refusal}
					rec, herr := r.resolveErrorRecovery(ctx, "model_refusal", r.opts.Exec.ErrorHandlers.ModelRefusal, refErr, agent,
						originalInput, concatRunItems(preStepItems, newStepItems), []*ModelResponse{resp})
					if herr != nil {
						return nil, herr
					}
					if rec == nil {
						return nil, refErr
					}
					if rec.message != nil {
						newStepItems = append(newStepItems, rec.message)
					}
					return &singleStepResult{NewStepItems: newStepItems, NextStep: stepFinalOutput, FinalOutput: rec.finalOutput}, nil
				}
			}
			if outputSchema != nil && !outputSchema.IsPlainText() {
				var final any
				if text != "" {
					var err error
					final, err = outputSchema.ValidateJSON(text)
					if err != nil {
						mbErr := NewModelBehaviorError("failed to parse structured output: %v", err)
						rec, herr := r.resolveErrorRecovery(ctx, "invalid_final_output", r.opts.Exec.ErrorHandlers.InvalidFinalOutput, mbErr, agent,
							originalInput, concatRunItems(preStepItems, newStepItems), []*ModelResponse{resp})
						if herr != nil {
							return nil, herr
						}
						if rec == nil {
							return nil, mbErr
						}
						if rec.message != nil {
							newStepItems = append(newStepItems, rec.message)
						}
						final = rec.finalOutput
					}
				} else {
					// No final text for a structured output type: recover via the
					// handler, or run the model again — never a hard failure.
					mbErr := NewModelBehaviorError("model returned no final output for the structured output type")
					rec, herr := r.resolveErrorRecovery(ctx, "invalid_final_output", r.opts.Exec.ErrorHandlers.InvalidFinalOutput, mbErr, agent,
						originalInput, concatRunItems(preStepItems, newStepItems), []*ModelResponse{resp})
					if herr != nil {
						return nil, herr
					}
					if rec == nil {
						return &singleStepResult{NewStepItems: newStepItems, NextStep: stepRunAgain}, nil
					}
					if rec.message != nil {
						newStepItems = append(newStepItems, rec.message)
					}
					final = rec.finalOutput
				}
				return &singleStepResult{NewStepItems: newStepItems, NextStep: stepFinalOutput, FinalOutput: final}, nil
			}
			// Plain text: the message text (or "", when the model produced
			// nothing actionable at all) is the final output.
			return &singleStepResult{NewStepItems: newStepItems, NextStep: stepFinalOutput, FinalOutput: text}, nil
		}
	}

	// Otherwise, feed tool results back to the model for another turn.
	return &singleStepResult{NewStepItems: newStepItems, NextStep: stepRunAgain}, nil
}

// orderToolResults merges the executed and rejected tool results back into the
// original call order given by calls, so run items and the turn hooks observe
// every call in the sequence the model emitted. Results are matched by call
// id; any unmatched executed/rejected results are appended defensively.
func orderToolResults(calls []toolRunFunction, executed, rejected []functionToolResult) []functionToolResult {
	byCallID := make(map[string]functionToolResult, len(executed)+len(rejected))
	for _, r := range executed {
		byCallID[r.callID] = r
	}
	for _, r := range rejected {
		byCallID[r.callID] = r
	}
	out := make([]functionToolResult, 0, len(byCallID))
	seen := make(map[string]bool, len(byCallID))
	for _, c := range calls {
		if res, ok := byCallID[c.Call.CallID]; ok && !seen[c.Call.CallID] {
			out = append(out, res)
			seen[c.Call.CallID] = true
		}
	}
	// Defensive: include any result whose call id was not in calls (should not
	// happen, but never drop a produced output).
	for _, r := range append(executed, rejected...) {
		if !seen[r.callID] {
			out = append(out, r)
			seen[r.callID] = true
		}
	}
	return out
}

// dropCompletedResumedCalls removes function calls whose function_call_output
// already exists among priorItems (the run's already-generated items). On the
// first turn after a HITL resume the interrupted model response is re-processed,
// so sibling calls that finished before the pause reappear as pending work; a
// completed call must be neither re-run (duplicating side effects) nor re-output
// (a duplicate call id the Responses API rejects). Its output was recorded
// before the pause, so dropping the call is safe. Mirrors
// openai-agents-python 3229/3259.
func dropCompletedResumedCalls(functions []toolRunFunction, priorItems []*RunItem) []toolRunFunction {
	if len(functions) == 0 {
		return functions
	}
	completed := map[string]struct{}{}
	for _, it := range priorItems {
		if id, _, isOutput := runItemCallID(it); isOutput {
			completed[id] = struct{}{}
		}
	}
	if len(completed) == 0 {
		return functions
	}
	out := make([]toolRunFunction, 0, len(functions))
	for _, f := range functions {
		if _, done := completed[f.Call.CallID]; done {
			continue
		}
		out = append(out, f)
	}
	return out
}

// concatRunItems returns a fresh slice of pre followed by post, for the
// RunErrorData snapshot handed to error handlers.
func concatRunItems(pre, post []*RunItem) []*RunItem {
	out := make([]*RunItem, 0, len(pre)+len(post))
	out = append(out, pre...)
	out = append(out, post...)
	return out
}

// allTerminate reports whether every tool in the batch asked the run to stop.
// An empty batch does not.
func allTerminate(results []functionToolResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, res := range results {
		if !res.terminate {
			return false
		}
	}
	return true
}

// coerceToolFinalOutput renders a tool's output as the run's final output when
// a tool asks to terminate, or a turn hook stops the run. For a plain-text
// agent (no output type) the value is coerced to a string so the final output
// is a string rather than a raw Go value. Agents with an output type keep the
// raw value for the caller to decode.
func coerceToolFinalOutput(agent *Agent, output any) any {
	if agent.OutputType != nil {
		return output
	}
	if s, ok := output.(string); ok {
		return s
	}
	return fmt.Sprint(output)
}
