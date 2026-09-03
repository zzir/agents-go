package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
)

// runNestedAgent executes (or resumes) the nested run, collecting without an
// OnStream handler and forwarding each event with one.
func runNestedAgent(ctx context.Context, a *Agent, input string, paused *RunState, opts RunOptions, cfg AgentToolConfig, tc *ToolContext, argsJSON string) (*RunResult, error) {
	// Stream when anyone is watching: an OnStream handler, or a streamed parent
	// whose consumer should see the sub-agent working (spec §2.7g).
	if cfg.OnStream == nil && !tc.streaming() {
		if paused != nil {
			return ResumeRunSync(ctx, paused, opts)
		}
		return RunSync(ctx, a, input, opts)
	}

	var stream RunStream
	if paused != nil {
		stream, _ = ResumeRun(ctx, paused, opts)
	} else {
		stream, _ = Run(ctx, a, input, opts)
	}

	// Callbacks dispatch from a background goroutine so a slow handler does not
	// stall the run; canceled drains the backlog without invoking it.
	var canceled atomic.Bool
	events := make(chan AgentToolStreamEvent, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if canceled.Load() {
				continue
			}
			dispatchAgentToolStreamEvent(ctx, cfg.OnStream, ev)
		}
	}()

	current := a
	var res *RunResult
	var runErr error
	for ev, eerr := range stream {
		if eerr != nil {
			runErr = eerr
			break
		}
		switch e := ev.(type) {
		case *AgentUpdatedStreamEvent:
			if e.NewAgent != nil {
				current = e.NewAgent
			}
		case *RunCompletedEvent:
			res = e.Result
			// The completion is the loop's own bookkeeping, not something a
			// handler watching the nested agent should see.
			continue
		case *RunItemStreamEvent:
			// Forward the nested agent's messages to the PARENT's stream as tool
			// progress; messages only, or raw deltas bury the parent's stream.
			if e.Item.Kind == ItemMessage {
				tc.Emit(TextResult(e.Item.Text()).WithDetails(map[string]any{
					"nested_agent": current.Name, "partial": true,
				}))
			}
		}
		if ctx.Err() != nil {
			// Parent canceled: keep draining so the run can finish and record
			// its result, but stop dispatching to the handler.
			canceled.Store(true)
			continue
		}
		if cfg.OnStream == nil {
			// Streaming only to forward progress; there is no handler to feed.
			continue
		}
		payload := AgentToolStreamEvent{
			Event:      ev,
			Agent:      current,
			ToolCallID: tc.ToolCallID,
			ToolName:   tc.ToolName,
			Arguments:  argsJSON,
		}
		select {
		case events <- payload:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		canceled.Store(true)
	}
	close(events)
	if ctx.Err() == nil {
		// Normal completion waits for the handler backlog to drain;
		// cancellation does not (the canceled flag makes the drain a no-op).
		<-done
	}
	if res == nil && runErr == nil {
		// The stream ended without completing — cancellation.
		runErr = ctx.Err()
	}
	return res, runErr
}

// dispatchAgentToolStreamEvent invokes OnStream, recovering a panic into a
// diagnostic on the parent run so a handler bug never fails the tool call.
func dispatchAgentToolStreamEvent(ctx context.Context, fn func(AgentToolStreamEvent), ev AgentToolStreamEvent) {
	defer func() {
		if p := recover(); p != nil {
			RecordDiagnostic(ctx, DiagToolPanic, newToolPanicError(ev.ToolName, p), map[string]any{
				"tool": ev.ToolName, "call_id": ev.ToolCallID, "source": "on_stream_handler",
			})
		}
	}()
	fn(ev)
}

// nestedRunInterrupt is returned by an agent-as-tool's OnInvoke when its
// nested run paused for approval; the parent pauses and caches state by callID.
type nestedRunInterrupt struct {
	callID        string
	state         *RunState
	interruptions []*ToolApprovalItem
}

func (e *nestedRunInterrupt) Error() string {
	return fmt.Sprintf("agent tool call %q paused for nested approval", e.callID)
}

// agentToolOutput derives a completed nested run's string result: the final
// output, else the last message text, else the last string tool output.
func agentToolOutput(res *RunResult) string {
	if s, ok := res.FinalOutput.(string); ok {
		if s != "" {
			return s
		}
	} else if res.FinalOutput != nil {
		// A structured final output: render it as its JSON payload rather than a
		// Go %v rendering, so the calling model receives valid data.
		if b, err := json.Marshal(res.FinalOutput); err == nil {
			return string(b)
		}
		return res.FinalOutputString()
	}
	for _, it := range slices.Backward(res.NewItems) {
		switch it.Kind {
		case ItemMessage:
			if t := it.Text(); t != "" {
				return t
			}
		case ItemToolCallOutput:
			if s, ok := it.Output.(string); ok && s != "" {
				return s
			}
		}
	}
	return res.FinalOutputString()
}

// nestedRunOptions builds a nested run's options: the parent's model, tracer,
// logger, guardrails and side-effect-free exec options; no Session, fresh approvals.
func nestedRunOptions(parent *RunContext) RunOptions {
	var opts RunOptions
	if parent != nil && parent.inheritedOpts != nil {
		opts.Model.Provider = parent.inheritedOpts.Model.Provider
		opts.Model.Override = parent.inheritedOpts.Model.Override
		opts.Model.Settings = parent.inheritedOpts.Model.Settings
		opts.Observe.Tracer = parent.inheritedOpts.Observe.Tracer
		opts.Exec.MaxToolConcurrency = parent.inheritedOpts.Exec.MaxToolConcurrency
		opts.Exec.ToolNotFoundBehavior = parent.inheritedOpts.Exec.ToolNotFoundBehavior
		opts.Exec.PreApprovalToolInputGuardrails = parent.inheritedOpts.Exec.PreApprovalToolInputGuardrails
		// Inherit the sensitive-data gate so a parent that disabled span
		// content cannot have it re-enabled by a nested agent-as-tool run.
		opts.Observe.IncludeSensitiveData = parent.inheritedOpts.Observe.IncludeSensitiveData
		// The log config travels too, keeping the two sensitive-data gates
		// consistent for the hardest part of the workflow to see into.
		opts.Log = parent.inheritedOpts.Log
		// Inherit input filters so a nested run's own handoffs and model calls
		// see the same rewriting the parent configured.
		opts.Exec.HandoffInputFilter = parent.inheritedOpts.Exec.HandoffInputFilter
		opts.Model.InputFilter = parent.inheritedOpts.Model.InputFilter
		// Inherit run-level guardrails so a nested run is guarded like its parent.
		opts.Guardrails = parent.inheritedOpts.Guardrails
	}
	if parent != nil {
		// Join the parent's trace so the nested run's spans are attributed to
		// the same workflow instead of an orphan root trace.
		opts.parentTrace = parent.activeTrace
	}
	// Share the parent's user context value, but use a fresh usage accumulator
	// and approval store for the nested run.
	if parent != nil {
		opts.Context = parent.Context
	}
	return opts
}
