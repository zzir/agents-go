package agents

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/zzir/agents-go/tracing"

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
	Tool Tool
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
	functionMap := make(map[string]Tool)
	for _, t := range tools {
		if _, ok := ToolAs[InvokableTool](t); ok {
			functionMap[t.ToolName()] = t
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
			// An item type this SDK does not model. Keeping it is not optional:
			// the Responses API gains types faster than any client tracks them,
			// and dropping one corrupts the conversation, because the next turn
			// resends a history the model does not recognize as its own.
			//
			// UnknownOutputItem carries the bytes through untouched. It used to
			// be silently discarded here.
			pr.NewItems = append(pr.NewItems, &UnknownOutputItem{Agent: agent, Raw: output})
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
		r.ctrl.setPhase(PhaseToolExecution)
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
						mbErr := newModelBehaviorError("failed to parse structured output: %v", err)
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
					// handler, or run the model again (never a hard failure —
					// Python parity).
					mbErr := newModelBehaviorError("model returned no final output for the structured output type")
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
	tool                Tool
	outputItem          *ToolCallOutputItem
	output              any
	callID              string
	nestedInterruptions []*ToolApprovalItem
	nestedState         *RunState
	// usage is what the tool spent on model calls of its own (an agent-as-tool's
	// nested run, a summarization step). Nil when the tool called no model.
	usage *Usage
	// terminate is the tool asking the run to stop after this batch. It only
	// takes effect when every tool in the batch asks.
	terminate bool
	// addedTools are deferred tools this result discloses.
	addedTools []string
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
	if limit := r.toolConcurrency(runs); limit > 0 {
		g.SetLimit(limit)
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
			// Progress is delivered only on a streamed run; on a blocking run
			// there is nobody watching, and buffering it would grow without
			// bound for a consumer that will never read it.
			if r.rawEvents {
				tc.emit = func(partial ToolResult) {
					r.emit(&ToolProgressEvent{
						ToolName: run.Call.Name,
						CallID:   run.Call.CallID,
						Agent:    agent,
						Result:   partial,
					})
				}
			}
			defer tc.finish()

			tlog := r.log.component("tool").with(
				slog.String("tool", run.Call.Name), slog.String("call_id", run.Call.CallID))
			tlog.Debug(ctx, "tool started", Sensitive("arguments", run.Call.Arguments))
			started := time.Now()
			defer func() {
				if err != nil {
					tlog.Error(ctx, "tool failed",
						slog.Duration("elapsed", time.Since(started)),
						slog.String("error", err.Error()))
					return
				}
				tlog.Debug(ctx, "tool finished", slog.Duration("elapsed", time.Since(started)))
			}()
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
				RecordDiagnostic(ctx, DiagToolPanic, perr, map[string]any{
					"tool": run.Call.Name, "call_id": run.Call.CallID,
				})
				if !toolHandlesFailure(run.Tool) {
					err = perr.fatalError()
					return
				}
				msg, herr := toolHandleFailure(gctx, run.Tool, tc, perr)
				if herr != nil {
					// The failure handler panicked too. Report it instead of
					// letting the second panic unwind the process.
					err = herr
					return
				}
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
			// The function span becomes the parent for anything the tool does:
			// an MCP round trip, a sandbox exec. Those receive a context and
			// nothing else belonging to the run.
			result, err := invokeTool(tracing.WithSpan(gctx, span), run.Tool, tc, run.Call.Arguments)
			out := result.ModelOutput()
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
				if !toolHandlesFailure(run.Tool) {
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
				var herr error
				out, herr = toolHandleFailure(gctx, run.Tool, tc, err)
				if herr != nil {
					// The handler itself panicked: fatal, like a tool without one.
					return herr
				}
				// A handled failure is still a failure as far as the UI is
				// concerned; the model sees the message either way.
				result = TextResult(stringifyToolOutput(out))
				result.IsError = true
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

			outputItem := newFunctionCallOutputItem(agent, run.Call.CallID, out)
			// The tool's own view of its call: UI data, renderer, error flag.
			// These come straight from the result — the tool knew all of it when
			// it returned, so there is no second pass to run and nothing for a
			// consumer to patch in afterwards.
			details, derr := normalizeDetails(result.Details)
			if derr != nil {
				return fmt.Errorf("tool %q: %w", run.Call.Name, derr)
			}
			outputItem.Extra = details
			outputItem.Renderer = result.Display
			outputItem.IsError = result.IsError
			outputItem.NestedUsage = result.Usage
			results[i] = functionToolResult{
				callID:     run.Call.CallID,
				tool:       run.Tool,
				outputItem: outputItem,
				output:     out,
				usage:      result.Usage,
				terminate:  result.Terminate,
				addedTools: result.AddedTools,
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
// part in the turn's tool results.
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
					msg = cmp.Or(msg, DefaultRejectionMessage)
					rejected = append(rejected, rejectResult(run, msg))
				} else {
					toRun = append(toRun, run)
				}
				continue
			}
		}
		needs, aerr := toolNeedsApproval(ctx, run.Tool, r.rc, run.Call.Arguments, run.Call.CallID)
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
		if r.opts.Exec.PreApprovalToolInputGuardrails {
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
func (r *runner) toolGuardrails(agent *Agent, tool Tool) []Guardrail {
	runLevel := r.runGuardrails(agent)
	if len(runLevel) == 0 {
		return toolOwnGuardrails(tool)
	}
	own := toolOwnGuardrails(tool)
	out := make([]Guardrail, 0, len(runLevel)+len(own))
	out = append(out, runLevel...)
	out = append(out, own...)
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
func invokeTool(ctx context.Context, tool Tool, tc *ToolContext, argsJSON string) (ToolResult, error) {
	invoker, ok := ToolAs[InvokableTool](tool)
	if !ok {
		return ToolResult{}, newUserError("tool %q cannot be invoked", tool.ToolName())
	}
	timeout := time.Duration(0)
	if tt, ok := ToolAs[TimeoutTool](tool); ok {
		timeout = tt.ToolTimeout()
	}
	if timeout <= 0 {
		return invoker.Invoke(ctx, tc, argsJSON)
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type invokeOutcome struct {
		out ToolResult
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
				ch <- invokeOutcome{err: &toolPanicError{toolName: tool.ToolName(), value: p, stack: debug.Stack()}}
			}
		}()
		out, err := invoker.Invoke(tctx, tc, argsJSON)
		ch <- invokeOutcome{out: out, err: err}
	}()

	timeoutErr := func() error {
		return &ToolTimeoutError{
			AgentsError: AgentsError{Code: CodeToolTimeout, Message: fmt.Sprintf("tool %q timed out after %v", tool.ToolName(), timeout)},
			ToolName:    tool.ToolName(),
		}
	}
	select {
	case res := <-ch:
		// A cooperative tool that returned because our deadline canceled tctx
		// reports the same timeout as the deadline branch below. An unrelated
		// error that merely lands near the deadline passes through unchanged.
		if res.err != nil && errors.Is(res.err, context.DeadlineExceeded) &&
			tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return ToolResult{}, timeoutErr()
		}
		return res.out, res.err
	case <-tctx.Done():
		// Only the tool's own deadline (not a caller cancellation) is a timeout.
		if ctx.Err() != nil {
			return ToolResult{}, ctx.Err()
		}
		return ToolResult{}, timeoutErr()
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
	return r.opts.Exec.HandoffInputFilter
}

func lastMessageItem(items []RunItem) *MessageOutputItem {
	for i := len(items) - 1; i >= 0; i-- {
		if m, ok := items[i].(*MessageOutputItem); ok {
			return m
		}
	}
	return nil
}

// toolOwnGuardrails returns whatever guardrails a tool declares, from a field
// or from a decorator — the runner does not need to know which.
func toolOwnGuardrails(tool Tool) []Guardrail {
	if g, ok := ToolAs[GuardedTool](tool); ok {
		return g.ToolGuardrails()
	}
	return nil
}

// toolHandlesFailure reports whether a tool converts its own errors into output
// the model can recover from. A tool that does not aborts the run.
func toolHandlesFailure(tool Tool) bool {
	h, ok := ToolAs[FailureHandlingTool](tool)
	if !ok {
		return false
	}
	// A FunctionTool always satisfies the interface but may have no handler
	// installed, which is how a caller asks for fatal errors.
	if ft, isFn := h.(*FunctionTool); isFn {
		return ft.FailureErrorFunction != nil
	}
	return true
}

// toolHandleFailure invokes the tool's failure handler. The handler is user
// code and gets the same panic protection the tool body has: one of its call
// sites is the recovery defer itself, where an unrecovered second panic would
// unwind straight out of the errgroup goroutine and kill the process — after
// the deferred done() had already let the run "complete". A panicking handler
// is reported as the fatal error it is, never re-thrown.
func toolHandleFailure(ctx context.Context, tool Tool, tc *ToolContext, cause error) (msg string, fatal error) {
	defer func() {
		if p := recover(); p != nil {
			perr := &toolPanicError{toolName: tool.ToolName(), value: p, stack: debug.Stack()}
			RecordDiagnostic(ctx, DiagToolPanic, perr, map[string]any{
				"tool": tool.ToolName(), "source": "failure_handler",
			})
			fatal = perr.fatalError()
		}
	}()
	if h, ok := ToolAs[FailureHandlingTool](tool); ok {
		return h.HandleToolFailure(ctx, tc, cause), nil
	}
	return "", nil
}

// toolNeedsApproval asks a tool whether this specific call needs human
// approval, from a field or a decorator.
func toolNeedsApproval(ctx context.Context, tool Tool, rc *RunContext, argsJSON, callID string) (bool, error) {
	if a, ok := ToolAs[ApprovalRequiredTool](tool); ok {
		return a.NeedsToolApproval(ctx, rc, argsJSON, callID)
	}
	return false, nil
}
