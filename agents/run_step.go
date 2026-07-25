package agents

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
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
	// NestedStates carries the paused RunState of any agent-as-tool nested run
	// that interrupted this turn, keyed by the parent tool call id, so the
	// runner can stash it on the parent RunState for ResumeRun.
	NestedStates map[string]*RunState
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
// appended a second time. originalInput, preStepItems and resp feed the
// RunErrorData snapshot when an ErrorHandlers recovery fires.
func (r *runner) executeToolsAndSideEffects(
	ctx context.Context,
	agent *Agent,
	pr *processedResponse,
	outputSchema OutputSchema,
	resumed bool,
	originalInput []TResponseInputItem,
	preStepItems []RunItem,
	resp *ModelResponse,
) (*singleStepResult, error) {
	newStepItems := make([]RunItem, 0, len(pr.NewItems))
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
	toRun, interruptions, rejected, err := r.partitionByApproval(ctx, agent, functions)
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

	// Run the approved/no-approval-needed function tools in parallel, then merge
	// with the rejected results in original call order so item order and
	// ToolUseBehavior see every call (Python parity: results built in tool_runs
	// order).
	executed, err := r.runFunctionTools(ctx, agent, toRun)
	if err != nil {
		return nil, err
	}
	functionResults := orderToolResults(functions, executed, rejected)

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
	// states so ResumeRun continues them. Sibling tools that completed keep
	// their outputs in newStepItems (Python parity: their FunctionToolResults
	// are recorded; only the interrupted call's output is withheld).
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
		// Attach the tool-not-found error to the current agent span (Python
		// parity: attach_error_to_current_span with {"tool_name":...}). The tool
		// name is model-chosen metadata, not user data, so it is recorded
		// regardless of the sensitive-data setting.
		r.agentSpan.SetError("Tool not found", map[string]any{"tool_name": call.Name})
		msg := fmt.Sprintf("Tool '%s' not found.", call.Name)
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

	// Determine whether the model produced a final output this turn. The branch
	// structure mirrors Python's execute_tools_and_side_effects tail.
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
				if refusal := extractMessageRefusal(lastMessage.Raw); refusal != "" {
					refErr := &ModelRefusalError{
						AgentsError: AgentsError{Code: CodeModelRefusal, Message: "model refused to respond: " + refusal},
						Refusal:     refusal,
					}
					rec, herr := r.resolveErrorRecovery(ctx, r.opts.ErrorHandlers.ModelRefusal, refErr, agent,
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
						mbErr := newModelBehaviorError("failed to parse structured output: %v", err)
						rec, herr := r.resolveErrorRecovery(ctx, r.opts.ErrorHandlers.InvalidFinalOutput, mbErr, agent,
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
					// handler, or run the model again (never a hard failure —
					// Python parity).
					mbErr := newModelBehaviorError("model returned no final output for the structured output type")
					rec, herr := r.resolveErrorRecovery(ctx, r.opts.ErrorHandlers.InvalidFinalOutput, mbErr, agent,
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
// original call order given by calls, so run items and ToolUseBehavior observe
// every call in the sequence the model emitted (Python builds its
// FunctionToolResults in tool_runs order). Results are matched by call id; any
// unmatched executed/rejected results are appended defensively.
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
func dropCompletedResumedCalls(functions []toolRunFunction, priorItems []RunItem) []toolRunFunction {
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
func concatRunItems(pre, post []RunItem) []RunItem {
	out := make([]RunItem, 0, len(pre)+len(post))
	out = append(out, pre...)
	out = append(out, post...)
	return out
}

// functionToolResult bundles a tool's output item with the tool and raw value.
// When an agent-as-tool's nested run paused for approval, outputItem is nil and
// the nested fields carry the surfaced interruptions and the paused nested
// state (keyed by this call's id) so the parent run can pause and later resume.
type functionToolResult struct {
	tool                *FunctionTool
	outputItem          *ToolCallOutputItem
	output              any
	callID              string
	nestedInterruptions []*ToolApprovalItem
	nestedState         *RunState
}

// toolPanicError is what a panic recovered from user tool code (the tool
// function, its guardrails, or hook callbacks) is converted into. Error() is
// deliberately a single line — a FailureErrorFunction feeds it back to the
// model like any other tool error, and the model does not need a goroutine
// stack — while the captured stack is appended only on the fatal path (see
// fatalError), where the run aborts and an operator is debugging.
type toolPanicError struct {
	toolName string
	value    any
	stack    []byte
}

func (e *toolPanicError) Error() string {
	return fmt.Sprintf("tool %q panicked: %v", e.toolName, e.value)
}

// fatalError formats the panic for the run-aborting path, appending the stack
// captured at recover time and classifying it as CodeToolPanic. The panic
// itself stays in the chain, so errors.As still reaches *toolPanicError.
func (e *toolPanicError) fatalError() error {
	return &AgentsError{
		Code:    CodeToolPanic,
		Message: fmt.Sprintf("%v\n\n%s", e, e.stack),
		cause:   e,
	}
}

// runFunctionTools invokes every function tool call concurrently, returning
// results in the original call order. Hook callbacks fire around each call.
func (r *runner) runFunctionTools(ctx context.Context, agent *Agent, runs []toolRunFunction) ([]functionToolResult, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	results := make([]functionToolResult, len(runs))
	// fatalErrs records each call's aborting error by call index. errgroup still
	// cancels the siblings on the first failure, but the error surfaced to the
	// run is chosen deterministically — the lowest call index — rather than
	// whichever goroutine happened to finish first (Python parity: a stable
	// winner by call order).
	fatalErrs := make([]error, len(runs))
	g, gctx := errgroup.WithContext(ctx)
	if r.opts.MaxToolConcurrency > 0 {
		g.SetLimit(r.opts.MaxToolConcurrency)
	}
	for i, run := range runs {
		g.Go(func() (err error) {
			// Record this call's aborting error (if any) by index for the
			// deterministic pick below. Registered first so it runs last —
			// after the panic-recovery defer may have set err.
			defer func() {
				if err != nil {
					fatalErrs[i] = err
				}
			}()
			tc := &ToolContext{
				RunContext:    r.rc,
				ToolName:      run.Call.Name,
				ToolCallID:    run.Call.CallID,
				ToolArguments: run.Call.Arguments,
				Agent:         agent,
				ToolCall:      run.Call.Raw,
			}
			// This goroutine runs user code (the tool itself, its guardrails,
			// hook callbacks) and errgroup does not recover panics, so an
			// unrecovered panic here would kill the whole process. Convert a
			// panic into the tool's regular error path instead: a
			// FailureErrorFunction feeds it back to the model, otherwise it
			// aborts the run (with the stack attached for debugging).
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				perr := &toolPanicError{toolName: run.Call.Name, value: p, stack: debug.Stack()}
				if run.Tool.FailureErrorFunction == nil {
					err = perr.fatalError()
					return
				}
				msg := run.Tool.FailureErrorFunction(gctx, tc, perr)
				results[i] = functionToolResult{
					tool:       run.Tool,
					outputItem: newFunctionCallOutputItem(agent, run.Call.CallID, msg),
					output:     msg,
					callID:     run.Call.CallID,
				}
				err = nil
			}()

			// Tool input guardrails run BEFORE the OnToolStart hooks (Python
			// parity): a reject_content guardrail resolves the call with a
			// substituted output and fires no tool hooks at all.
			if rejected, msg, err := r.runToolStage(gctx, agent, StageToolInput, run, nil); err != nil {
				return err
			} else if rejected {
				results[i] = functionToolResult{
					tool:       run.Tool,
					outputItem: newFunctionCallOutputItem(agent, run.Call.CallID, msg),
					output:     msg,
					callID:     run.Call.CallID,
				}
				return nil
			}

			if err := callToolStart(gctx, r.opts.Hooks, agent, tc, run.Tool); err != nil {
				return err
			}

			span := r.trace.StartFunctionSpan(run.Call.Name, r.agentParentID())
			defer span.Finish() // idempotent; covers panics in user tool code
			if span.Span != nil {
				tc.functionSpanID = span.Span.SpanID
			}
			// Record the call arguments and result on the span (Python parity:
			// FunctionSpanData.input/output), gated like generation payloads.
			logToolData := r.traceIncludeSensitiveData()
			if logToolData {
				span.Set("input", run.Call.Arguments)
			}
			out, err := invokeTool(gctx, run.Tool, tc, run.Call.Arguments)
			if err != nil {
				// An agent-as-tool whose nested run paused for approval is not a
				// failure: record the surfaced interruptions and the paused
				// nested state (no output item) so the parent run pauses too.
				if ni, ok := errors.AsType[*nestedRunInterrupt](err); ok {
					span.Finish()
					results[i] = functionToolResult{
						tool:                run.Tool,
						callID:              run.Call.CallID,
						nestedInterruptions: ni.interruptions,
						nestedState:         ni.state,
					}
					return nil
				}
				// Tool errors routinely embed the call arguments, so the span
				// error is gated like input/output (Python parity:
				// get_trace_tool_error / REDACTED_TOOL_ERROR_MESSAGE).
				if logToolData {
					span.SetError(err.Error(), nil)
				} else {
					span.SetError(redactedToolErrorMessage, nil)
				}
				span.Finish()
				// Without a FailureErrorFunction the error aborts the run.
				if run.Tool.FailureErrorFunction == nil {
					// A panic recovered inside invokeTool's timeout goroutine
					// gets its stack attached here, like the direct path above.
					if pe, ok := errors.AsType[*toolPanicError](err); ok {
						err = pe.fatalError()
					}
					return fmt.Errorf("tool %q failed: %w", run.Call.Name, err)
				}
				// The failure message becomes the tool output and flows through
				// the same tail as a success — output guardrails, custom data,
				// and OnToolEnd all see it (Python parity: the error is converted
				// inside the invocation, then handled like a normal result).
				out = run.Tool.FailureErrorFunction(gctx, tc, err)
			} else {
				if logToolData {
					span.Set("output", stringifyToolOutput(out))
				}
				span.Finish()
			}

			// Common tail for success and handled-error outputs.
			// Tool output guardrails: may reject (substitute content) or raise.
			if rejected, msg, err := r.runToolStage(gctx, agent, StageToolOutput, run, out); err != nil {
				return err
			} else if rejected {
				out = msg
			}

			if err := callToolEnd(gctx, r.opts.Hooks, agent, tc, run.Tool, out); err != nil {
				return err
			}
			outputItem := newFunctionCallOutputItem(agent, run.Call.CallID, out)
			// SDK-only custom data: extracted after guardrails from the final
			// model-visible output, attached to the run item but never to the
			// replayed input item.
			if run.Tool.CustomDataExtractor != nil {
				data, cerr := run.Tool.CustomDataExtractor(gctx, FunctionToolCustomDataContext{
					ToolContext: tc,
					Tool:        run.Tool,
					Output:      out,
					RawItem:     outputItem.Raw,
				})
				if cerr != nil {
					return fmt.Errorf("tool %q custom data extractor failed: %w", run.Call.Name, cerr)
				}
				if outputItem.CustomData, cerr = normalizeCustomData(data); cerr != nil {
					return cerr
				}
			}
			results[i] = functionToolResult{
				callID:     run.Call.CallID,
				tool:       run.Tool,
				outputItem: outputItem,
				output:     out,
			}
			return nil
		})
	}
	if werr := g.Wait(); werr != nil {
		// Prefer the lowest-index call's error so the reported failure is stable
		// across runs regardless of goroutine scheduling.
		for _, fe := range fatalErrs {
			if fe != nil {
				return nil, fe
			}
		}
		return nil, werr
	}
	return results, nil
}

// DefaultRejectionMessage is sent back to the model when a tool call is rejected
// without a custom message.
const DefaultRejectionMessage = "Tool execution was not approved."

// redactedToolErrorMessage replaces a tool error's text on its function span
// when sensitive-data tracing is off — error strings routinely embed the call
// arguments. Matches Python's REDACTED_TOOL_ERROR_MESSAGE.
const redactedToolErrorMessage = "Tool execution failed. Error details are redacted."

// partitionByApproval splits function tool calls into those ready to run, those
// awaiting human approval (interruptions), and rejected calls. It consults the
// run context's ApprovalStore. Rejected calls come back as functionToolResults
// (output = rejection message) so they keep their place in call order and take
// part in ToolUseBehavior, matching Python.
func (r *runner) partitionByApproval(ctx context.Context, agent *Agent, runs []toolRunFunction) (toRun []toolRunFunction, interruptions []*ToolApprovalItem, rejected []functionToolResult, err error) {
	rejectResult := func(run toolRunFunction, msg string) functionToolResult {
		return functionToolResult{
			tool:       run.Tool,
			outputItem: newFunctionCallOutputItem(agent, run.Call.CallID, msg),
			output:     msg,
			callID:     run.Call.CallID,
		}
	}
	store := r.rc.Approvals
	for _, run := range runs {
		// An explicit approve/reject decision (typically on resume) wins before
		// anything else: honoring it here skips re-invoking NeedsApprovalFunc,
		// whose side effects or errors must not re-fire for a resolved call
		// (Python parity: openai-agents-python 3229/3259).
		if store != nil {
			if decision, decided := store.decisionFor(run.Call.Name, run.Call.CallID); decided {
				if !decision.approved {
					msg := decision.message
					if msg == "" {
						msg = DefaultRejectionMessage
					}
					rejected = append(rejected, rejectResult(run, msg))
				} else {
					toRun = append(toRun, run)
				}
				continue
			}
		}
		needs, aerr := run.Tool.requiresApproval(ctx, r.rc, run.Call.Arguments, run.Call.CallID)
		if aerr != nil {
			return nil, nil, nil, aerr
		}
		if !needs {
			needs = agentApprovesToolName(agent, run.Call.Name)
		}
		if !needs {
			toRun = append(toRun, run)
			continue
		}
		// Pre-approval tool input guardrails (opt-in): run the tool's input
		// guardrails before surfacing the approval interruption, so a guardrail
		// rejection resolves the call without a human round-trip. Calls that
		// pass still re-run the same guardrails right before execution after
		// approval, so time-sensitive checks are revalidated on resume.
		if r.opts.PreApprovalToolInputGuardrails {
			preRejected, msg, gerr := r.runToolStage(ctx, agent, StageToolInput, run, nil)
			if gerr != nil {
				return nil, nil, nil, gerr
			}
			if preRejected {
				rejected = append(rejected, rejectResult(run, msg))
				continue
			}
		}
		interruptions = append(interruptions, &ToolApprovalItem{
			Agent:     agent,
			ToolName:  run.Call.Name,
			CallID:    run.Call.CallID,
			Arguments: run.Call.Arguments,
			Raw:       run.Call.Raw,
		})
	}
	return toRun, interruptions, rejected, nil
}

func agentApprovesToolName(agent *Agent, toolName string) bool {
	for _, name := range agent.ApproveTools {
		if name == "*" || name == toolName {
			return true
		}
	}
	return false
}

// toolGuardrails is the guardrail set consulted for one tool call: run-level
// and agent-level guardrails first (their tool stages cover every tool), then
// the tool's own.
func (r *runner) toolGuardrails(agent *Agent, tool *FunctionTool) []Guardrail {
	runLevel := r.runGuardrails(agent)
	if len(runLevel) == 0 {
		return tool.Guardrails
	}
	out := make([]Guardrail, 0, len(runLevel)+len(tool.Guardrails))
	out = append(out, runLevel...)
	out = append(out, tool.Guardrails...)
	return out
}

// runToolStage runs the guardrails covering stage for one tool call. It returns
// (replaced, replacementMessage, error): replaced means the call's result is
// the message — at StageToolInput the tool is skipped, at StageToolOutput its
// result is substituted. An error halts the run.
//
// Guardrails run in order and stop at the first Replace: once one has
// substituted the content, running the rest against the original is meaningless.
// Every consulted guardrail's result is recorded, allowing decisions included,
// so callers can read each one's OutputInfo.
func (r *runner) runToolStage(ctx context.Context, agent *Agent, stage GuardrailStage, run toolRunFunction, output any) (bool, string, error) {
	results, msg, replaced, err := runStageSequential(ctx, r.rc, r.toolGuardrails(agent, run.Tool), GuardrailPayload{
		Stage:      stage,
		Agent:      agent,
		ToolName:   run.Call.Name,
		ToolCallID: run.Call.CallID,
		Arguments:  run.Call.Arguments,
		Output:     output,
	})
	r.recordGuardrailResults(results...)
	if err != nil {
		return false, "", err
	}
	return replaced, msg, nil
}

// invokeTool runs a tool's OnInvoke, enforcing FunctionTool.Timeout when set.
//
// With a timeout, OnInvoke runs in its own goroutine so the deadline holds
// even for tools that never check their context: when the deadline fires,
// invokeTool returns a *ToolTimeoutError immediately, while the tool goroutine
// keeps running in the background until OnInvoke returns on its own. Its late
// result (or panic) is delivered to a buffered channel private to this call
// and discarded — it never touches shared state. Cancellation of the caller's
// ctx is reported as ctx.Err(), never as a timeout.
func invokeTool(ctx context.Context, tool *FunctionTool, tc *ToolContext, argsJSON string) (any, error) {
	if tool.OnInvoke == nil {
		return nil, newUserError("function tool %q has no OnInvoke", tool.Name)
	}
	if tool.Timeout <= 0 {
		return tool.OnInvoke(ctx, tc, argsJSON)
	}
	tctx, cancel := context.WithTimeout(ctx, tool.Timeout)
	defer cancel()

	type invokeOutcome struct {
		out any
		err error
	}
	// Buffered so the goroutine can always deliver its (possibly late) result
	// and exit, even after the select below has returned on the timeout.
	ch := make(chan invokeOutcome, 1)
	go func() {
		// This goroutine runs user tool code; like runFunctionTools, recover a
		// panic (it would otherwise kill the process) and surface it as an
		// error on the tool's normal error path.
		defer func() {
			if p := recover(); p != nil {
				ch <- invokeOutcome{err: &toolPanicError{toolName: tool.Name, value: p, stack: debug.Stack()}}
			}
		}()
		out, err := tool.OnInvoke(tctx, tc, argsJSON)
		ch <- invokeOutcome{out: out, err: err}
	}()

	timeoutErr := func() error {
		return &ToolTimeoutError{
			AgentsError: AgentsError{Code: CodeToolTimeout, Message: fmt.Sprintf("tool %q timed out after %v", tool.Name, tool.Timeout)},
			ToolName:    tool.Name,
		}
	}
	select {
	case res := <-ch:
		// A cooperative tool that returned because our deadline canceled tctx
		// reports the same timeout as the deadline branch below. An unrelated
		// error that merely lands near the deadline passes through unchanged.
		if res.err != nil && errors.Is(res.err, context.DeadlineExceeded) &&
			tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, timeoutErr()
		}
		return res.out, res.err
	case <-tctx.Done():
		// Only the tool's own deadline (not a caller cancellation) is a timeout.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, timeoutErr()
	}
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
	span := r.trace.StartHandoffSpan(run.Handoff.ToolName, r.agentParentID())
	defer span.Finish()
	// Validate the handoff arguments against the handoff's input schema before it
	// fires, so a handoff that expects input but receives none (or invalid input)
	// is rejected as a *ModelBehaviorError instead of silently transferring with
	// zero-valued input (Python parity: handoffs/__init__.py:278-307).
	if verr := validateHandoffInput(&run.Handoff, run.Call.Arguments); verr != nil {
		span.SetError(verr.Error(), map[string]any{"details": "invalid handoff input"})
		return nil, verr
	}
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
		return true, coerceToolFinalOutput(agent, results[0].output), nil
	case StopAtTools:
		for _, res := range results {
			if slices.Contains(b.Names, res.tool.Name) {
				return true, coerceToolFinalOutput(agent, res.output), nil
			}
		}
		return false, nil, nil
	case ToolUseBehaviorFunc:
		public := make([]FunctionToolResult, len(results))
		for i, res := range results {
			var custom map[string]any
			if res.outputItem != nil {
				custom = res.outputItem.CustomData
			}
			public[i] = FunctionToolResult{ToolName: res.tool.Name, Output: res.output, CustomData: custom}
		}
		stop, output, err := b(ctx, r.rc, public)
		return stop, output, err
	default:
		return false, nil, nil
	}
}

// coerceToolFinalOutput renders a tool's output as the run's final output when
// ToolUseBehavior stops the run. For a plain-text agent (no output type) the
// value is coerced to a string so the final output is a string, not a raw Go
// value — matching Python's `str(final_output)` for str/plain-text agents.
// Agents with an output type keep the raw value for the caller to decode.
func coerceToolFinalOutput(agent *Agent, output any) any {
	if agent.OutputType != nil {
		return output
	}
	if s, ok := output.(string); ok {
		return s
	}
	return fmt.Sprint(output)
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
