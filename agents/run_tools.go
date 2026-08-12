package agents

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/zzir/agents-go/tracing"
)

// functionToolResult bundles a tool's output item with the tool and raw value.
// When an agent-as-tool's nested run paused for approval, outputItem is nil and
// the nested fields carry the surfaced interruptions and the paused nested
// state (keyed by this call's id) so the parent run can pause and later resume.
type functionToolResult struct {
	tool                *Tool
	outputItem          *RunItem
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

// toolPanicError is what a panic recovered from user tool code is converted
// into. Error() is deliberately a single line — a FailureErrorFunction feeds it
// back to the model like any other tool error — while the captured stack is
// appended only on the fatal path (see fatalError). It is built at three recover
// points: the per-tool errgroup goroutine, invokeTool's timeout goroutine, and
// toolHandleFailure.
type toolPanicError struct {
	toolName string
	value    any
	stack    []byte
}

// newToolPanicError captures the recovered panic value and the stack at the
// recover point. Call it directly inside the recovering defer so the stack
// still shows the panic's origin frames.
func newToolPanicError(toolName string, p any) *toolPanicError {
	return &toolPanicError{toolName: toolName, value: p, stack: debug.Stack()}
}

func (e *toolPanicError) Error() string {
	return fmt.Sprintf("tool %q panicked: %v", e.toolName, e.value)
}

// fatalError formats the panic for the run-aborting path, appending the stack
// captured at recover time and classifying it as CodeToolPanic. The panic
// itself stays in the chain, so errors.As still reaches *toolPanicError.
func (e *toolPanicError) fatalError() error {
	return &codedError{
		code:  CodeToolPanic,
		msg:   fmt.Sprintf("%v\n\n%s", e, e.stack),
		cause: e,
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
	// whichever goroutine happened to finish first.
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
			// nobody is watching and buffering it would grow without bound.
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
			// This goroutine runs user code and errgroup does not recover
			// panics, so convert a panic into the tool's regular error path: a
			// FailureErrorFunction feeds it back to the model, else it aborts
			// the run (with the stack attached).
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				perr := newToolPanicError(run.Call.Name, p)
				RecordDiagnostic(ctx, DiagToolPanic, perr, map[string]any{
					"tool": run.Call.Name, "call_id": run.Call.CallID,
				})
				if run.Tool.FailureErrorFunction == nil {
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

			// Tool input guardrails run BEFORE the tool executes: a
			// reject_content guardrail resolves the call with a substituted
			// output and the tool itself never runs.
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
			// The call id is what ties this span back to the conversation item
			// the tool produced — an id, not payload, so it is recorded whether
			// or not sensitive data is.
			span.Set("call_id", run.Call.CallID)
			// Record the call arguments and result on the span, gated like generation
			// payloads.
			logToolData := r.traceIncludeSensitiveData()
			if logToolData {
				span.Set("input", run.Call.Arguments)
			}
			// The function span becomes the parent for anything the tool does —
			// an MCP round trip, a sandbox exec — via the context.
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
				// A panic recovered inside invokeTool's timeout goroutine
				// arrives here as a regular error. Record the same diagnostic
				// the direct-path recover records, so a panic is observable
				// whether or not the tool had a Timeout.
				if pe, ok := errors.AsType[*toolPanicError](err); ok {
					RecordDiagnostic(ctx, DiagToolPanic, pe, map[string]any{
						"tool": run.Call.Name, "call_id": run.Call.CallID,
					})
				}
				// A timeout is recorded whether or not a FailureErrorFunction
				// converts it into model-visible output below.
				if te, ok := errors.AsType[*ToolTimeoutError](err); ok {
					RecordDiagnostic(ctx, DiagToolTimeout, te, map[string]any{
						"tool": run.Call.Name, "call_id": run.Call.CallID,
					})
				}
				// Tool errors routinely embed the call arguments, so the span error is
				// gated like input/output.
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
				// The failure message becomes the tool output and flows through the same
				// tail as a success — output guardrails and custom data see it.
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
			// The tool's own view of its call: UI data, renderer, error flag,
			// straight from the result.
			details, derr := normalizeDetails(result.Details)
			if derr != nil {
				return fmt.Errorf("tool %q: %w", run.Call.Name, derr)
			}
			outputItem.Extra = details
			outputItem.Renderer = result.Display
			outputItem.Title = result.Title
			outputItem.Summary = result.Summary
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
		// Surface the first cause, not the first casualty: pick the lowest-index
		// error, but skip a context.Canceled that is only errgroup cancelling a
		// sibling after another tool's failure. Cancellation wins only when it is
		// all there is (the consumer abandoned the run mid-batch).
		var cancelled error
		for _, fe := range fatalErrs {
			if fe == nil {
				continue
			}
			if !errors.Is(fe, context.Canceled) {
				return nil, fe
			}
			cancelled = cmp.Or(cancelled, fe)
		}
		if cancelled != nil {
			return nil, cancelled
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
// arguments.
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
		// anything else: honoring it here skips re-invoking NeedsApprovalFunc, whose
		// side effects or errors must not re-fire for a resolved call.
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
		needs, aerr := run.Tool.needsApproval(ctx, r.rc, run.Call.Arguments, run.Call.CallID)
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
		// Pre-approval tool input guardrails (opt-in): run them before surfacing
		// the interruption, so a rejection resolves the call without a human
		// round-trip. Passing calls re-run them after approval too.
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
func (r *runner) toolGuardrails(agent *Agent, tool *Tool) []Guardrail {
	runLevel := r.runGuardrails(agent)
	if len(runLevel) == 0 {
		return tool.Guardrails
	}
	own := tool.Guardrails
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
// Guardrails run in order and stop at the first Replace. Every consulted
// guardrail's result is recorded, allowing decisions included, so callers can
// read each one's OutputInfo.
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

// invokeTool runs a tool's OnInvoke, enforcing Tool.Timeout when set.
//
// With a timeout, OnInvoke runs in its own goroutine so the deadline holds
// even for tools that never check their context: when the deadline fires,
// invokeTool returns a *ToolTimeoutError immediately, while the tool goroutine
// keeps running in the background until OnInvoke returns on its own. Its late
// result (or panic) is delivered to a buffered channel private to this call
// and discarded — it never touches shared state. Cancellation of the caller's
// ctx is reported as ctx.Err(), never as a timeout.
func invokeTool(ctx context.Context, tool *Tool, tc *ToolContext, argsJSON string) (ToolResult, error) {
	if tool.OnInvoke == nil {
		return ToolResult{}, NewUserError("tool %q has no OnInvoke", tool.Name)
	}
	timeout := tool.Timeout
	if timeout <= 0 {
		return tool.OnInvoke(ctx, tc, argsJSON)
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
				ch <- invokeOutcome{err: newToolPanicError(tool.Name, p)}
			}
		}()
		out, err := tool.OnInvoke(tctx, tc, argsJSON)
		ch <- invokeOutcome{out: out, err: err}
	}()

	timeoutErr := func() error {
		return &ToolTimeoutError{ToolName: tool.Name, Timeout: timeout}
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

// toolHandleFailure invokes the tool's failure handler. The handler is user code
// and gets the same panic protection the tool body has — one call site is the
// recovery defer itself — so a panicking handler is reported as the fatal error
// it is, never re-thrown.
func toolHandleFailure(ctx context.Context, tool *Tool, tc *ToolContext, cause error) (msg string, fatal error) {
	defer func() {
		if p := recover(); p != nil {
			perr := newToolPanicError(tool.Name, p)
			RecordDiagnostic(ctx, DiagToolPanic, perr, map[string]any{
				"tool": tool.Name, "source": "failure_handler",
			})
			fatal = perr.fatalError()
		}
	}()
	if tool.FailureErrorFunction != nil {
		return tool.FailureErrorFunction(ctx, tc, cause), nil
	}
	return "", nil
}
