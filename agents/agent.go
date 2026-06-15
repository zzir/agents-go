package agents

import "context"

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

	// InputGuardrails run on the original input before the agent executes.
	InputGuardrails []InputGuardrail

	// OutputGuardrails run on the agent's final output.
	OutputGuardrails []OutputGuardrail

	// OutputType, when non-nil, requests structured output validated against the
	// schema. A nil OutputType yields plain text.
	OutputType OutputSchema

	// Hooks receives agent-scoped lifecycle callbacks.
	Hooks AgentHooks

	// ToolUseBehavior controls post-tool-call behavior. Nil means RunLLMAgain.
	ToolUseBehavior ToolUseBehavior

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
