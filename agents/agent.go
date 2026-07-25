package agents

import (
	"context"
	"fmt"
	"strings"
)

// Instructions produces the system prompt for an agent. It may be static or
// computed per run. Use StaticInstructions for a fixed string or InstructionsFunc
// to derive instructions dynamically from the run context.
type Instructions interface {
	GetInstructions(ctx context.Context, rc *RunContext, agent *Agent) (string, error)
}

type staticInstructions string

func (s staticInstructions) GetInstructions(context.Context, *RunContext, *Agent) (string, error) {
	return string(s), nil
}

// StaticInstructions wraps a fixed instruction string as Instructions.
func StaticInstructions(s string) Instructions { return staticInstructions(s) }

type instructionsFunc func(context.Context, *RunContext, *Agent) (string, error)

func (f instructionsFunc) GetInstructions(ctx context.Context, rc *RunContext, agent *Agent) (string, error) {
	return f(ctx, rc, agent)
}

// InstructionsFunc adapts a function to the Instructions interface.
func InstructionsFunc(f func(context.Context, *RunContext, *Agent) (string, error)) Instructions {
	return instructionsFunc(f)
}

// wrappedInstructions decorates an Instructions with a prefix and/or suffix,
// applied at resolution time. It eliminates the common pattern of extracting
// text with GetInstructions(ctx, nil, nil), concatenating, and re-wrapping.
type wrappedInstructions struct {
	inner          Instructions
	prefix, suffix string
}

func (w *wrappedInstructions) GetInstructions(ctx context.Context, rc *RunContext, agent *Agent) (string, error) {
	base, err := w.inner.GetInstructions(ctx, rc, agent)
	if err != nil {
		return "", err
	}
	if w.prefix != "" && base != "" {
		base = w.prefix + "\n\n" + base
	} else if w.prefix != "" {
		base = w.prefix
	}
	if w.suffix != "" && base != "" {
		base = base + "\n\n" + w.suffix
	} else if w.suffix != "" {
		base = w.suffix
	}
	return base, nil
}

// WrapInstructions decorates inner by prepending prefix and appending suffix at
// resolution time, separated by double newlines. Empty prefix/suffix are
// skipped. If inner is nil, a static instruction from prefix+suffix is returned.
func WrapInstructions(inner Instructions, prefix, suffix string) Instructions {
	if inner == nil {
		s := prefix
		if suffix != "" {
			if s != "" {
				s += "\n\n" + suffix
			} else {
				s = suffix
			}
		}
		return StaticInstructions(s)
	}
	return &wrappedInstructions{inner: inner, prefix: prefix, suffix: suffix}
}

// ToolUseBehavior controls what happens after the model calls one or more tools.
// It is a sealed interface; use the predefined implementations below.
type ToolUseBehavior interface {
	toolUseBehavior()
}

// RunLLMAgain feeds tool results back to the model for another turn. This is the
// default behavior.
type RunLLMAgain struct{}

func (RunLLMAgain) toolUseBehavior() {}

// StopOnFirstTool stops the run and uses the first tool's output as the final
// result, without another model call.
type StopOnFirstTool struct{}

func (StopOnFirstTool) toolUseBehavior() {}

// StopAtTools stops the run if any tool whose name is in Names is called.
type StopAtTools struct {
	Names []string
}

func (StopAtTools) toolUseBehavior() {}

// ParseToolUseBehavior converts a configuration string to a ToolUseBehavior.
// Recognized formats: "" or "run_llm_again" → nil, "stop_on_first" →
// StopOnFirstTool{}, "stop_at:name1,name2" → StopAtTools{Names: [...]}.
// Unknown values return nil (RunLLMAgain default).
func ParseToolUseBehavior(s string) ToolUseBehavior {
	switch {
	case s == "" || s == "run_llm_again":
		return nil
	case s == "stop_on_first":
		return StopOnFirstTool{}
	case strings.HasPrefix(s, "stop_at:"):
		raw := strings.TrimPrefix(s, "stop_at:")
		parts := strings.Split(raw, ",")
		var names []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				names = append(names, p)
			}
		}
		return StopAtTools{Names: names}
	default:
		return nil
	}
}

// ToolUseBehaviorFunc decides, from the results of the tools run this turn,
// whether to stop with a final output. Returning stop=true ends the run with the
// given output; stop=false runs the LLM again. It mirrors the callable form of
// Python's tool_use_behavior.
type ToolUseBehaviorFunc func(ctx context.Context, rc *RunContext, results []FunctionToolResult) (stop bool, output any, err error)

func (ToolUseBehaviorFunc) toolUseBehavior() {}

// Agent is a model configured with instructions, tools, guardrails, handoffs and
// an optional structured output type. It is the central building block of the
// SDK and mirrors the Python SDK's Agent dataclass.
//
// Construct an Agent with a struct literal; only Name is required. Zero values
// are sensible defaults (e.g. a nil ToolUseBehavior means RunLLMAgain).
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
	Tools []Tool

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
	// These are per-AGENT, which is why they survived the removal of the hook
	// interfaces: a handoff swaps the agent, and with it these callbacks, in a
	// way run-level middleware cannot express. Everything else the old
	// eight-method interfaces observed is now on the event stream or in a
	// guardrail, both of which can also rewrite rather than only refuse.
	OnStart func(ctx context.Context, rc *RunContext) error
	// OnEnd runs after this agent produces the run's final output.
	OnEnd func(ctx context.Context, rc *RunContext, output any) error

	// ToolUseBehavior controls post-tool-call behavior. Nil means RunLLMAgain.
	ToolUseBehavior ToolUseBehavior

	// ApproveTools lists tool names that require human approval before
	// execution, overriding each tool's own NeedsApproval field. A single
	// entry of "*" means every tool requires approval.
	ApproveTools []string

	// DisableToolChoiceReset keeps ModelSettings.ToolChoice as-is on every turn.
	// By default (false), once this agent has called a tool, tool_choice is left
	// unset on its subsequent turns so a "required" or specific-tool setting
	// cannot force an infinite tool-call loop. It is the inverse of the Python
	// SDK's reset_tool_choice (default true).
	DisableToolChoiceReset bool
}

// Clone returns a shallow copy of the agent. Callers can mutate the returned
// agent's fields without affecting the original. Slices and maps are shared, so
// replace (rather than append to) them when customizing.
func (a *Agent) Clone() *Agent {
	cp := *a
	return &cp
}

// GetSystemPrompt resolves the agent's instructions for the given run context.
func (a *Agent) GetSystemPrompt(ctx context.Context, rc *RunContext) (string, error) {
	if a.Instructions == nil {
		return "", nil
	}
	return a.Instructions.GetInstructions(ctx, rc, a)
}

// GetPrompt resolves the agent's stored-prompt configuration for the given run
// context, or nil if the agent has no Prompt.
func (a *Agent) GetPrompt(ctx context.Context, rc *RunContext) (*Prompt, error) {
	if a.Prompt == nil {
		return nil, nil
	}
	p, err := a.Prompt.GetPrompt(ctx, rc, a)
	if err != nil {
		return nil, err
	}
	if p != nil && p.ID == "" {
		return nil, fmt.Errorf("agent %q: prompt ID is required", a.Name)
	}
	return p, nil
}
