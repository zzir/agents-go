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
	// NestedStates carries the paused RunState of each agent-as-tool nested run
	// that interrupted this turn, keyed by parent tool call id.
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

// ParseToolNotFoundBehavior converts a string to a ToolNotFoundBehavior:
// "error" (or ""), "return_to_model" or "return_error_to_model"; else error.
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
	// A tool with no OnInvoke fails AT INVOCATION as a UserError; filtering it
	// here would route the call to not-found and blame the model.
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
			// An item type this SDK does not model: dropping it would corrupt the
			// next turn's history, so ItemUnknown carries the bytes through.
			pr.NewItems = append(pr.NewItems, NewModelItem(ItemUnknown, agent, output))
		}
	}
	return pr, nil
}

// stepProgress is the run as this turn found it: the input it started from,
// the items generated before it, and the response being executed.
type stepProgress struct {
	originalInput []InputItem
	preStepItems  []*RunItem
	resp          *ModelResponse
}

// executeToolsAndSideEffects runs the tools and handoffs a model response
// requested and decides the next step; resumed skips re-appending its items.
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

	// A resume re-processes the interrupted response; sibling calls that
	// completed before the pause are dropped (dropCompletedResumedCalls).
	functions := pr.Functions
	if resumed {
		functions = dropCompletedResumedCalls(functions, prog.preStepItems)
	}

	// Partition calls by approval — except on a truncated response, whose
	// calls must neither run nor ASK: all go to truncatedCallResults (§2.7e).
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

	// Run the approved tools in parallel, then merge with the rejected results
	// in original call order so items and hooks see every call.
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
			// A nested run paused for approval: gather its interruptions and
			// cache its state by call id; its output is withheld until resume.
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

	// A paused nested run pauses the parent too, surfacing its interruptions
	// as the parent's own; completed siblings keep their outputs.
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
		// Data, not SetError: the model recovers next turn, so the run is not
		// failed. The name is model-chosen metadata, recorded regardless.
		r.agentSpan.Set("tool_not_found", names)
	}

	// Handoffs take precedence: switch to the first requested target agent.
	if len(handoffs) > 0 {
		return r.executeHandoff(ctx, agent, handoffs, newStepItems)
	}

	// Every tool in the batch asked to stop: honor it with the last output.
	// Unanimity, not "any" — spec §2.3c.
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

// decideFinalOutput reads a tool-free turn's last message as the final output;
// a refusal or a bad structured output goes through ErrorHandlers — spec §2.3.
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

// orderToolResults merges executed and rejected results back into the model's
// call order, matched by call id.
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

// dropCompletedResumedCalls removes calls whose output already exists among
// priorItems: on a resume they must be neither re-run nor re-output.
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

// coerceToolFinalOutput renders a tool's output as the run's final output: the raw
// value for an agent with an output type, else a string via stringifyToolOutput.
func coerceToolFinalOutput(agent *Agent, output any) any {
	if agent.OutputType != nil {
		return output
	}
	if s, ok := output.(string); ok {
		return s
	}
	return stringifyToolOutput(output)
}
