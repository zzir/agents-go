package agents

import "context"

// Prompt configures an agent to use an OpenAI stored prompt (the Responses API
// `prompt` parameter) instead of, or alongside, inline Instructions.
type Prompt struct {
	// ID is the stored prompt's unique identifier. Required.
	ID string
	// Version optionally pins a specific version of the prompt template.
	Version string
	// Variables optionally substitutes values into the prompt template. String
	// values are sent as text substitutions; non-string values are stringified.
	Variables map[string]any
}

// PromptProvider yields an agent's stored-prompt configuration per run. A nil
// *Prompt result means the agent uses no stored prompt for that run.
//
// It is a func type: assign a function directly, or use StaticPrompt for a
// fixed configuration. (It was an interface once, with only unexported
// implementations — an adapter layer nothing ever plugged into.)
type PromptProvider func(ctx context.Context, rc *RunContext, agent *Agent) (*Prompt, error)

// StaticPrompt returns a PromptProvider yielding a fixed Prompt.
func StaticPrompt(p Prompt) PromptProvider {
	return func(context.Context, *RunContext, *Agent) (*Prompt, error) {
		pp := p
		return &pp, nil
	}
}
