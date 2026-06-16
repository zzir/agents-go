package agents

import "context"

// Prompt configures an agent to use an OpenAI stored prompt (the Responses API
// `prompt` parameter) instead of, or alongside, inline Instructions. It is the
// Go counterpart of the Python SDK's Prompt.
type Prompt struct {
	// ID is the stored prompt's unique identifier. Required.
	ID string
	// Version optionally pins a specific version of the prompt template.
	Version string
	// Variables optionally substitutes values into the prompt template. String
	// values are sent as text substitutions; non-string values are stringified.
	Variables map[string]any
}

// PromptProvider yields an agent's stored-prompt configuration, either fixed
// (StaticPrompt) or computed per run (PromptFunc). A nil *Prompt result means
// the agent uses no stored prompt for that run.
type PromptProvider interface {
	GetPrompt(ctx context.Context, rc *RunContext, agent *Agent) (*Prompt, error)
}

type staticPrompt Prompt

func (p staticPrompt) GetPrompt(context.Context, *RunContext, *Agent) (*Prompt, error) {
	pp := Prompt(p)
	return &pp, nil
}

// StaticPrompt wraps a fixed Prompt as a PromptProvider.
func StaticPrompt(p Prompt) PromptProvider { return staticPrompt(p) }

type promptFunc func(context.Context, *RunContext, *Agent) (*Prompt, error)

func (f promptFunc) GetPrompt(ctx context.Context, rc *RunContext, agent *Agent) (*Prompt, error) {
	return f(ctx, rc, agent)
}

// PromptFunc adapts a function to the PromptProvider interface, for prompts that
// depend on the run context (the Go counterpart of Python's DynamicPromptFunction).
func PromptFunc(f func(context.Context, *RunContext, *Agent) (*Prompt, error)) PromptProvider {
	return promptFunc(f)
}
