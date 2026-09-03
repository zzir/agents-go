package agents

import (
	"context"
	"fmt"
)

// Instructions produces the system prompt for an agent, computed per run.
//
// It is a func type: assign a function directly for dynamic instructions, or
// use StaticInstructions for a fixed string.
type Instructions func(ctx context.Context, rc *RunContext, agent *Agent) (string, error)

// StaticInstructions returns Instructions yielding a fixed string.
func StaticInstructions(s string) Instructions {
	return func(context.Context, *RunContext, *Agent) (string, error) { return s, nil }
}

// WrapInstructions decorates inner by prepending prefix and appending suffix at
// resolution time (so per-run inner instructions still compose), separated by
// double newlines. Empty prefix/suffix are skipped; a nil inner contributes
// nothing.
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

// Agent is a model configured with instructions, tools, guardrails, handoffs and
// an optional structured output type. It is the central building block of the
// SDK: a plain struct with no Run method, so the runner takes it as data.
//
// Construct an Agent with a struct literal; only Name is required. Zero values
// are sensible defaults (e.g. a nil ModelSettings means the provider's).
type Agent struct {
	// Name identifies the agent. Required.
	Name string

	// HandoffDescription describes the agent when it is used as a handoff target.
	HandoffDescription string

	// Instructions is the system prompt. May be nil for no system prompt.
	Instructions Instructions

	// Prompt, when set, configures the agent to use an OpenAI stored prompt
	// (the Responses API `prompt` parameter). It is independent of Instructions;
	// both may be set. Only the OpenAI Responses backend honors it.
	Prompt PromptProvider

	// Handoffs are the sub-agents (or explicit Handoff values) this agent may
	// delegate to.
	Handoffs []Handoff

	// Model selects the LLM. If ModelImpl is non-nil it is used directly;
	// otherwise Model is treated as a model name resolved via the run's
	// ModelProvider. An empty Model with a nil ModelImpl uses the provider
	// default.
	Model string
	// ModelImpl is an explicit Model instance, taking precedence over Model.
	ModelImpl Model

	// ModelSettings overrides default model configuration for this agent.
	ModelSettings *ModelSettings

	// Tools are the function tools available to the agent.
	Tools []*Tool

	// MCPServers are MCP servers whose tools are exposed to the agent.
	MCPServers []MCPServer

	// Guardrails inspect the run at the stages each one declares. Guardrails
	// here cover the whole run: their tool stages apply to every tool the agent
	// exposes, not just one.
	Guardrails []Guardrail

	// OutputType, when non-nil, requests structured output validated against the
	// schema. A nil OutputType yields plain text.
	OutputType OutputSchema
	// OnStart runs before this agent takes a turn; returning an error aborts
	// the run. OnEnd runs after it produces the final output.
	//
	// These are per-AGENT: a handoff swaps the agent and with it these callbacks,
	// in a way run-level middleware cannot express.
	OnStart func(ctx context.Context, rc *RunContext) error
	// OnEnd runs after this agent produces the run's final output.
	OnEnd func(ctx context.Context, rc *RunContext, output any) error

	// ApproveTools lists tool names that require human approval before
	// execution, overriding each tool's own NeedsApproval field. A single
	// entry of "*" means every tool requires approval.
	ApproveTools []string

	// DisableToolChoiceReset keeps ModelSettings.ToolChoice as-is on every turn.
	// By default (false), once this agent has called a tool, tool_choice is left
	// unset on its subsequent turns so a "required" or specific-tool setting
	// cannot force an infinite tool-call loop.
	DisableToolChoiceReset bool
}

// Clone returns a shallow copy of the agent. Callers can mutate the returned
// agent's fields without affecting the original. Slices and maps are shared, so
// replace (rather than append to) them when customizing.
func (a *Agent) Clone() *Agent {
	cp := *a
	return &cp
}

// systemPrompt resolves the agent's instructions for the run: nil means no
// system prompt. The runner's per-turn resolution point.
func (a *Agent) systemPrompt(ctx context.Context, rc *RunContext) (string, error) {
	if a.Instructions == nil {
		return "", nil
	}
	return a.Instructions(ctx, rc, a)
}

// resolvePrompt resolves the agent's stored-prompt configuration for the run, or
// nil when the agent has none. A prompt without an ID is an error.
func (a *Agent) resolvePrompt(ctx context.Context, rc *RunContext) (*Prompt, error) {
	if a.Prompt == nil {
		return nil, nil
	}
	p, err := a.Prompt(ctx, rc, a)
	if err != nil {
		return nil, err
	}
	if p != nil && p.ID == "" {
		return nil, fmt.Errorf("agent %q: prompt ID is required", a.Name)
	}
	return p, nil
}
