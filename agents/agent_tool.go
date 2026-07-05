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
			var args agentToolInput
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, newModelBehaviorError("agent tool %q: invalid arguments: %v", name, err)
			}
			nestedOpts := nestedRunOptions(tc.RunContext, cfg.MaxTurns)
			// Parent the nested run's agent spans under this tool call's
			// function span so the trace tree shows which call owns the run.
			nestedOpts.parentSpanID = tc.functionSpanID
			res, err := Run(ctx, a, args.Input, nestedOpts)
			if err != nil {
				return nil, fmt.Errorf("agent tool %q run failed: %w", name, err)
			}
			if cfg.CustomOutputExtractor != nil {
				return cfg.CustomOutputExtractor(res)
			}
			return res.FinalOutputString(), nil
		},
	}
}

// nestedRunOptions builds the RunOptions for a nested run, inheriting the
// parent's model provider/model/tracer plus side-effect-free execution options
// (tool concurrency limit, tool-not-found behavior), but with a fresh approval
// store so nested approvals don't leak into the parent. Stateful options
// (Session, Hooks, guardrails, input filters) deliberately do not carry over.
func nestedRunOptions(parent *RunContext, maxTurns int) RunOptions {
	opts := RunOptions{MaxTurns: maxTurns}
	if parent != nil && parent.inheritedOpts != nil {
		opts.ModelProvider = parent.inheritedOpts.ModelProvider
		opts.Model = parent.inheritedOpts.Model
		opts.ModelSettings = parent.inheritedOpts.ModelSettings
		opts.Tracer = parent.inheritedOpts.Tracer
		opts.MaxToolConcurrency = parent.inheritedOpts.MaxToolConcurrency
		opts.ToolNotFoundBehavior = parent.inheritedOpts.ToolNotFoundBehavior
		// Inherit the sensitive-data gate so a parent that disabled span
		// content cannot have it re-enabled by a nested agent-as-tool run.
		opts.TraceIncludeSensitiveData = parent.inheritedOpts.TraceIncludeSensitiveData
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
