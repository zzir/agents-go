package agents

import (
	"context"
)

// Instructions produces the system prompt for an agent, computed per run;
// StaticInstructions wraps a fixed string.
type Instructions func(ctx context.Context, rc *RunContext, agent *Agent) (string, error)

// StaticInstructions returns Instructions yielding a fixed string.
func StaticInstructions(s string) Instructions {
	return func(context.Context, *RunContext, *Agent) (string, error) { return s, nil }
}

// WrapInstructions prepends prefix and appends suffix to inner at resolution
// time, separated by blank lines; empty parts and a nil inner are skipped.
func WrapInstructions(inner Instructions, prefix, suffix string) Instructions {
	return func(ctx context.Context, rc *RunContext, agent *Agent) (string, error) {
		base := ""
		if inner != nil {
			b, err := inner(ctx, rc, agent)
			if err != nil {
				return "", err
			}
			base = b
		}
		if prefix != "" && base != "" {
			base = prefix + "\n\n" + base
		} else if prefix != "" {
			base = prefix
		}
		if suffix != "" && base != "" {
			base = base + "\n\n" + suffix
		} else if suffix != "" {
			base = suffix
		}
		return base, nil
	}
}

// Agent is a model configured with instructions, tools, guardrails, handoffs
// and an optional structured output type: a plain struct the runner takes as
// data. Build one with a struct literal; only Name is required.
type Agent struct {
	// Name identifies the agent. Required.
	Name string

	// HandoffDescription describes the agent when it is used as a handoff target.
	HandoffDescription string

	// Instructions is the system prompt; nil means none.
	Instructions Instructions

	// Handoffs are the sub-agents (or explicit Handoff values) this agent may
	// delegate to.
	Handoffs []Handoff

	// Model is the model name resolved via the run's ModelProvider when
	// ModelImpl is nil; empty selects the provider default.
	Model string
	// ModelImpl is an explicit Model instance, taking precedence over Model.
	ModelImpl Model

	// ModelSettings overrides default model configuration for this agent.
	ModelSettings *ModelSettings

	// Tools are the function tools available to the agent.
	Tools []*Tool

	// MCPServers are MCP servers whose tools are exposed to the agent.
	MCPServers []MCPServer

	// Guardrails inspect the whole run at the stages each one declares; their
	// tool stages apply to every tool the agent exposes.
	Guardrails []Guardrail

	// OutputType, when non-nil, requests structured output validated against
	// the schema; nil yields plain text.
	OutputType OutputSchema
	// OnStart runs before this agent takes a turn; an error aborts the run.
	// A handoff swaps the agent and with it these callbacks.
	OnStart func(ctx context.Context, rc *RunContext) error
	// OnEnd runs after this agent produces the run's final output.
	OnEnd func(ctx context.Context, rc *RunContext, output any) error

	// ApproveTools lists tool names that require human approval, overriding
	// each tool's own NeedsApproval; a single "*" means every tool.
	ApproveTools []string

	// DisableToolChoiceReset keeps ModelSettings.ToolChoice on every turn. By
	// default tool_choice is unset after the agent's first tool call.
	DisableToolChoiceReset bool
}

// Clone returns a shallow copy of the agent; slices and maps are shared, so
// replace rather than append to them.
func (a *Agent) Clone() *Agent {
	cp := *a
	return &cp
}

// systemPrompt resolves the agent's instructions for the run; nil means none.
func (a *Agent) systemPrompt(ctx context.Context, rc *RunContext) (string, error) {
	if a.Instructions == nil {
		return "", nil
	}
	return a.Instructions(ctx, rc, a)
}
