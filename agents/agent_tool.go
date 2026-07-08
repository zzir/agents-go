package agents

import (
	"context"
	"encoding/json"
	"fmt"
)

// AgentToolConfig configures Agent.AsTool.
type AgentToolConfig struct {
	// Name is the tool name exposed to the calling agent. Defaults to the
	// agent's name (sanitized) when empty.
	Name string
	// Description tells the calling model what the tool does and when to use it.
	Description string
	// MaxTurns bounds the nested run. Zero uses DefaultMaxTurns.
	MaxTurns int
	// CustomOutputExtractor, when set, derives the tool's string result from the
	// nested run. By default the final output is used.
	CustomOutputExtractor func(*RunResult) (string, error)
}

// agentToolInput is the default argument schema for an agent tool: a single
// input string forwarded to the nested agent.
type agentToolInput struct {
	Input string `json:"input" jsonschema:"The input to pass to the agent"`
}

// AsTool turns the agent into a Tool callable by other agents. Unlike a handoff,
// the nested agent receives only the provided input (not the full conversation)
// and returns control to the calling agent when done. It mirrors the Python
// SDK's Agent.as_tool.
//
// The nested run inherits the parent run's model provider, model override and
// tracer from the run context, so the sub-agent need not set its own model when
// the parent supplies a provider.
func (a *Agent) AsTool(cfg AgentToolConfig) Tool {
	name := cfg.Name
	if name == "" {
		name = transformToolName(a.Name)
	}
	schema, err := SchemaFor[agentToolInput](true)
	if err != nil {
		schema = emptyStrictSchema()
	}
	return &FunctionTool{
		Name:                 name,
		Description:          cfg.Description,
		ParamsJSONSchema:     schema,
		Strict:               true,
		FailureErrorFunction: DefaultToolErrorFunction,
		OnInvoke: func(ctx context.Context, tc *ToolContext, argsJSON string) (any, error) {
			nestedOpts := nestedRunOptions(tc.RunContext, cfg.MaxTurns)
			// Parent the nested run's agent spans under this tool call's
			// function span so the trace tree shows which call owns the run.
			nestedOpts.parentSpanID = tc.functionSpanID

			var res *RunResult
			var err error
			// On resume, continue the nested run this call paused (human-in-the-
			// loop): its state was cached on the parent run context by tool call
			// id. Mirror the parent's approve/reject decisions for the surfaced
			// nested interruptions into the nested run before resuming so the
			// human's choice takes effect. Mirrors Python's as_tool resume path.
			if tc.RunContext != nil {
				if paused := tc.takeNestedToolState(tc.ToolCallID); paused != nil {
					if tc.Approvals != nil {
						tc.Approvals.mirrorInto(paused.Approvals, paused.Interruptions)
					}
					res, err = ResumeRun(ctx, paused, nestedOpts)
				}
			}
			if res == nil && err == nil {
				var args agentToolInput
				if uerr := json.Unmarshal([]byte(argsJSON), &args); uerr != nil {
					return nil, newModelBehaviorError("agent tool %q: invalid arguments: %v", name, uerr)
				}
				res, err = Run(ctx, a, args.Input, nestedOpts)
			}
			if err != nil {
				return nil, fmt.Errorf("agent tool %q run failed: %w", name, err)
			}
			// The nested run paused for approval: surface its interruptions to the
			// parent run (via the sentinel below) instead of returning an output,
			// so the parent pauses too and the human sees the nested approval.
			// Usage is not folded in yet — it will be when the resumed nested run
			// finally completes, carrying its full usage.
			if len(res.Interruptions) > 0 {
				return nil, &nestedRunInterrupt{
					callID:        tc.ToolCallID,
					state:         res.State,
					interruptions: res.Interruptions,
				}
			}
			// Fold the completed nested run's usage into the parent run's usage
			// (Python parity: the nested run shares the parent's usage). Add is
			// goroutine-safe, so concurrent agent-tool calls are fine.
			if tc.RunContext != nil && res.Usage != nil {
				tc.Usage.Add(res.Usage)
			}
			if cfg.CustomOutputExtractor != nil {
				return cfg.CustomOutputExtractor(res)
			}
			return agentToolOutput(res), nil
		},
	}
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
// mirroring Python's as_tool default extraction: the final output when
// non-empty, else the last non-empty assistant message text, else the last
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
	for i := len(res.NewItems) - 1; i >= 0; i-- {
		switch it := res.NewItems[i].(type) {
		case *MessageOutputItem:
			if t := it.Text(); t != "" {
				return t
			}
		case *ToolCallOutputItem:
			if s, ok := it.Output.(string); ok && s != "" {
				return s
			}
		}
	}
	return res.FinalOutputString()
}

// nestedRunOptions builds the RunOptions for a nested run, inheriting the
// parent's model provider/model/tracer plus side-effect-free execution options
// (tool concurrency limit, tool-not-found behavior, input filters), but with a
// fresh approval store so nested approvals don't leak into the parent. Stateful
// options (Session, Hooks, guardrails) deliberately do not carry over.
func nestedRunOptions(parent *RunContext, maxTurns int) RunOptions {
	opts := RunOptions{MaxTurns: maxTurns}
	if parent != nil && parent.inheritedOpts != nil {
		opts.ModelProvider = parent.inheritedOpts.ModelProvider
		opts.Model = parent.inheritedOpts.Model
		opts.ModelSettings = parent.inheritedOpts.ModelSettings
		opts.Tracer = parent.inheritedOpts.Tracer
		opts.MaxToolConcurrency = parent.inheritedOpts.MaxToolConcurrency
		opts.ToolNotFoundBehavior = parent.inheritedOpts.ToolNotFoundBehavior
		opts.PreApprovalToolInputGuardrails = parent.inheritedOpts.PreApprovalToolInputGuardrails
		// Inherit the sensitive-data gate so a parent that disabled span
		// content cannot have it re-enabled by a nested agent-as-tool run.
		opts.TraceIncludeSensitiveData = parent.inheritedOpts.TraceIncludeSensitiveData
		// Inherit input filters so a nested run's own handoffs and model calls
		// see the same rewriting the parent configured (Python parity: a nested
		// run with run_config=None reuses the parent's RunConfig).
		opts.HandoffInputFilter = parent.inheritedOpts.HandoffInputFilter
		opts.CallModelInputFilter = parent.inheritedOpts.CallModelInputFilter
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
