package agents

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
	// Every tool is dispatchable — one with no OnInvoke fails AT INVOCATION as a
	// UserError naming the tool. Filtering it out here would route the call to the
	// not-found path, blaming the model for a configuration bug.
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
			// dropping one corrupts the conversation, because the next turn resends
			// a history the model does not recognize. ItemUnknown carries the bytes
			// through untouched.
			pr.NewItems = append(pr.NewItems, NewModelItem(ItemUnknown, agent, output))
		}
	}
	return pr, nil
}

// stepProgress is the run as this turn found it: the input it started from, the
// items generated before this turn, and the response being executed. The three
// travel together because they are only ever read together.
type stepProgress struct {
	originalInput []InputItem
	preStepItems  []*RunItem
	resp          *ModelResponse
}

// executeToolsAndSideEffects runs the tools and handoffs requested by a model
// response and determines the next step.
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
	prog stepProgress,
) (*singleStepResult, error) {
	resp := prog.resp
	newStepItems := make([]*RunItem, 0, len(pr.NewItems))
	if !resumed {
		newStepItems = append(newStepItems, pr.NewItems...)
	}

	// On the first turn after a HITL resume the interrupted response is
	// re-processed, so sibling calls that completed before the pause reappear.
	// Drop them (see dropCompletedResumedCalls) so they are neither re-run nor
	// re-output.
	functions := pr.Functions
	if resumed {
		functions = dropCompletedResumedCalls(functions, prog.preStepItems)
	}

	// Human-in-the-loop: partition function calls into those ready to run, those
	// awaiting approval, and rejected calls (already resolved to results).
	//
	// A truncated response bypasses the partition: a call whose arguments may stop
	// mid-JSON must neither run nor ASK, since a resumed approval in another
	// process would execute what this one refuses. All fall through to
	// truncatedCallResults.
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

	// Run the approved function tools in parallel, then merge with the rejected
	// results in original call order so item order and the turn-boundary hooks
	// see every call.
	var executed, refusedHandoffs []functionToolResult
	handoffs := pr.Handoffs
	if resp.Truncated() {
		// None of the calls run, the handoffs included: each is answered with
		// a refusal and the model resends (spec §2.7e).
		r.log.component("tool").Warn(ctx, "response truncated; refusing to run its tool calls",
			slog.Int("calls", len(toRun)+len(handoffs)))
		RecordDiagnostic(ctx, DiagResponseTruncated, nil, map[string]any{
			"calls": len(toRun) + len(handoffs), "response_id": resp.ResponseID,
		})
		calls := make([]functionCall, 0, len(toRun))
		for _, run := range toRun {
			calls = append(calls, run.Call)
		}
		executed = truncatedCallResults(agent, calls)
		calls = calls[:0]
		for _, h := range handoffs {
			calls = append(calls, h.Call)
		}
		refusedHandoffs = truncatedCallResults(agent, calls)
		handoffs = nil
	} else {
		executed, err = r.runFunctionTools(ctx, agent, toRun)
		if err != nil {
			return nil, err
		}
	}
	functionResults := append(orderToolResults(functions, executed, rejected), refusedHandoffs...)

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
	if len(pr.UnknownTools) > 0 {
		names := make([]string, len(pr.UnknownTools))
		for i, call := range pr.UnknownTools {
			names[i] = call.Name
			newStepItems = append(newStepItems, newFunctionCallOutputItem(agent, call.CallID, fmt.Sprintf("Tool '%s' not found.", call.Name)))
		}
		// Record on the span as data, not SetError: the model is handed the
		// failure and recovers next turn, so the run is not failed — a red agent
		// span would misreport a run that completed. The name is model-chosen
		// metadata, not user data, so it is recorded regardless of the
		// sensitive-data setting.
		r.agentSpan.Set("tool_not_found", names)
	}

	// Handoffs take precedence: switch to the first requested target agent.
	if len(handoffs) > 0 {
		return r.executeHandoff(ctx, agent, handoffs, newStepItems)
	}

	// Every tool in the batch asked to stop: honor it, using the last output as
	// the final result. Unanimity is the rule, not "any" — stopping while another
	// tool is still working would throw away a result the model asked for.
	if allTerminate(functionResults) {
		return &singleStepResult{
			NewStepItems: newStepItems,
			NextStep:     stepFinalOutput,
			FinalOutput:  coerceToolFinalOutput(agent, functionResults[len(functionResults)-1].output),
		}, nil
	}

	// Determine whether the model produced a final output this turn.
	lastMessage := lastMessageItem(newStepItems)
	// Tool activity without any message (e.g. only rejected calls): the results
	// must go back to the model.
	hasToolActivityWithoutMessage := lastMessage == nil && len(pr.ToolsUsed) > 0
	if !pr.hasToolsToRun() && !hasToolActivityWithoutMessage {
		return r.decideFinalOutput(ctx, agent, outputSchema, lastMessage, newStepItems, prog)
	}

	// Otherwise, feed tool results back to the model for another turn.
	return &singleStepResult{NewStepItems: newStepItems, NextStep: stepRunAgain}, nil
}

// decideFinalOutput reads a tool-free turn's last message as the run's final
// output. newStepItems is the turn's items so far; a recovery handler's
// synthesized message joins them, which is why the whole step result is built
// here rather than just the output value.
//
// Three ways the message is not simply the answer, each recoverable through
// ExecOptions.ErrorHandlers instead of failing the run outright:
//
//   - a refusal, which outranks any text or structured content in the same
//     message;
//   - text that does not validate against a structured output type;
//   - no text at all for a structured output type — where a handler that
//     declines means running the model again, never a hard failure.
func (r *runner) decideFinalOutput(
	ctx context.Context,
	agent *Agent,
	outputSchema OutputSchema,
	lastMessage *RunItem,
	newStepItems []*RunItem,
	prog stepProgress,
) (*singleStepResult, error) {
	// recoverVia asks an ErrorHandlers hook to turn a failed final output into a
	// completed run, showing it the run's progress including this turn.
	recoverVia := func(kind string, handler RunErrorHandler, cause error) (*errorRecovery, error) {
		return r.resolveErrorRecovery(ctx, kind, handler, cause, agent,
			prog.originalInput, slices.Concat(prog.preStepItems, newStepItems), []*ModelResponse{prog.resp})
	}

	var text string
	if lastMessage != nil {
		text = lastMessage.Text()
		if refusal := lastMessage.refusal(); refusal != "" {
			refErr := &ModelRefusalError{Refusal: refusal}
			rec, herr := recoverVia("model_refusal", r.opts.Exec.ErrorHandlers.ModelRefusal, refErr)
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
	if outputSchema == nil || outputSchema.IsPlainText() {
		// Plain text: the message text (or "", when the model produced nothing
		// actionable at all) is the final output.
		return &singleStepResult{NewStepItems: newStepItems, NextStep: stepFinalOutput, FinalOutput: text}, nil
	}

	var final any
	if text != "" {
		var err error
		final, err = outputSchema.ValidateJSON(text)
		if err != nil {
			mbErr := NewModelBehaviorError("failed to parse structured output: %v", err)
			rec, herr := recoverVia("invalid_final_output", r.opts.Exec.ErrorHandlers.InvalidFinalOutput, mbErr)
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
		mbErr := NewModelBehaviorError("model returned no final output for the structured output type")
		rec, herr := recoverVia("invalid_final_output", r.opts.Exec.ErrorHandlers.InvalidFinalOutput, mbErr)
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

// orderToolResults merges the executed and rejected tool results back into the
// original call order given by calls, so run items and the turn hooks observe
// every call in the sequence the model emitted. Results are matched by call
// id; every result came from one of calls.
func orderToolResults(calls []toolRunFunction, executed, rejected []functionToolResult) []functionToolResult {
	byCallID := make(map[string]functionToolResult, len(executed)+len(rejected))
	for _, r := range executed {
		byCallID[r.callID] = r
	}
	for _, r := range rejected {
		byCallID[r.callID] = r
	}
	out := make([]functionToolResult, 0, len(byCallID))
	for _, c := range calls {
		if res, ok := byCallID[c.Call.CallID]; ok {
			out = append(out, res)
			delete(byCallID, c.Call.CallID)
		}
	}
	return out
}

// dropCompletedResumedCalls removes function calls whose function_call_output
// already exists among priorItems. On the first turn after a HITL resume the
// interrupted response is re-processed, so a call that finished before the pause
// must be neither re-run (duplicate side effects) nor re-output (a duplicate call
// id the Responses API rejects). Dropping it is safe: its output is already in
// the log.
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
//
// It renders through stringifyToolOutput so a multimodal output reads as the JSON
// the model was sent, not as Go syntax.
func coerceToolFinalOutput(agent *Agent, output any) any {
	if agent.OutputType != nil {
		return output
	}
	if s, ok := output.(string); ok {
		return s
	}
	return stringifyToolOutput(output)
}
