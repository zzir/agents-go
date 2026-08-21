package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
)

// runNestedAgent executes (or resumes) the nested run. Without an OnStream
// handler it collects; with one, it forwards each event to the handler as it
// arrives.
func runNestedAgent(ctx context.Context, a *Agent, input string, paused *RunState, opts RunOptions, cfg AgentToolConfig, tc *ToolContext, argsJSON string) (*RunResult, error) {
	// Stream the nested run when anyone is watching: a configured OnStream
	// handler, or a streamed parent whose consumer should see the sub-agent
	// working rather than a tool call that hangs for a minute.
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

	// Dispatch callbacks from a background goroutine so a slow handler does not
	// stall the run, which now executes on THIS goroutine.
	//
	// canceled mirrors the parent's cancellation: once it fires, backlogged
	// events drain without invoking the callback, so OnStream never fires after
	// the tool call has returned.
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
			// Forward the nested agent's messages to the PARENT run's stream as
			// tool progress, so a UI watching the parent sees the sub-agent
			// working. Messages only: relaying raw deltas would bury the
			// parent's own stream.
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
		// cancellation does not (the canceled flag makes the leftover drain a
		// no-op).
		<-done
	}
	if res == nil && runErr == nil {
		// The stream ended without completing — cancellation.
		runErr = ctx.Err()
	}
	return res, runErr
}

// dispatchAgentToolStreamEvent invokes the OnStream callback, recovering any
// panic: a handler bug must not fail the tool call. The panic is recorded as a
// diagnostic on the parent run — the handler is the parent's configuration
// — so it is observable instead of silently dropped.
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

// nestedRunInterrupt is returned by an agent-as-tool's OnInvoke when its nested
// run paused for human approval. The parent runner recognizes it (rather than
// treating it as a tool failure), surfaces the nested interruptions as the
// parent run's own, and caches state under callID so a ResumeRun continues the
// nested run.
type nestedRunInterrupt struct {
	callID        string
	state         *RunState
	interruptions []*ToolApprovalItem
}

func (e *nestedRunInterrupt) Error() string {
	return fmt.Sprintf("agent tool call %q paused for nested approval", e.callID)
}

// agentToolOutput derives the string result of a completed nested run,
// preferring the most specific thing it said: the final output when non-empty, else the last non-empty assistant message text, else the last
// non-empty string tool output, else the (stringified) final output.
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

// nestedRunOptions builds the RunOptions for a nested run, inheriting the
// parent's model provider/model/tracer/logger, run-level guardrails and
// side-effect-free execution options (tool concurrency limit, tool-not-found
// behavior, input filters). The nested run gets a fresh
// approval store so nested approvals don't leak into the parent. The parent's
// Session never carries over; cfg.ModifyRunOptions is the only way to give the
// nested run conversation state of its own.
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
		// Inherit the log configuration for the same reason as the tracer: the
		// nested run is the hardest part of the workflow to see into.
		// LogConfig.SensitiveData travels with it, keeping the two
		// sensitive-data gates consistent.
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
