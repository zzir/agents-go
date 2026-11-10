package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// runNestedAgent executes (or resumes) the nested run; under a streamed parent
// it forwards the nested agent's messages as tool progress (spec §2.7g).
func runNestedAgent(ctx context.Context, a *Agent, input string, paused *RunState, opts RunOptions, tc *ToolContext) (*RunResult, error) {
	if !tc.streaming() {
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

	current := a
	var res *RunResult
	for ev, err := range stream {
		if err != nil {
			return nil, err
		}
		switch e := ev.(type) {
		case *AgentUpdatedStreamEvent:
			if e.NewAgent != nil {
				current = e.NewAgent
			}
		case *RunCompletedEvent:
			res = e.Result
		case *RunItemStreamEvent:
			// Messages only: raw deltas would bury the parent's stream.
			if e.Item.Kind == ItemMessage {
				tc.Emit(TextResult(e.Item.Text()).WithDetails(map[string]any{
					"nested_agent": current.Name, "partial": true,
				}))
			}
		}
	}
	if res == nil {
		// The stream ended without completing — cancellation.
		return nil, ctx.Err()
	}
	return res, nil
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
		opts.Exec.ToolOutputLimit = parent.inheritedOpts.Exec.ToolOutputLimit
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
