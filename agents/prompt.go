package agents

import (
	"context"
	"maps"
)

// Prompt configures an agent to use an OpenAI stored prompt (the Responses API
// `prompt` parameter) instead of, or alongside, inline Instructions.
type Prompt struct {
	// ID is the stored prompt's unique identifier. Required.
	ID string
	// Version optionally pins a specific version of the prompt template.
	Version string
	// Variables optionally substitutes values into the prompt template. Only
	// string values are supported, as text substitutions: a provider rejects a
	// non-string value with a *UserError rather than stringifying it, because
	// an intended image or file variable would turn into garbage text. The
	// value type stays any so those content variables can be modeled later.
	Variables map[string]any
}

// PromptProvider yields an agent's stored-prompt configuration per run. A nil
// *Prompt result means the agent uses no stored prompt for that run.
//
// It is a func type: assign a function directly, or use StaticPrompt for a
// fixed configuration. (It was an interface once, with only unexported
// implementations — an adapter layer nothing ever plugged into.)
type PromptProvider func(ctx context.Context, rc *RunContext, agent *Agent) (*Prompt, error)

// StaticPrompt returns a PromptProvider yielding a fixed Prompt. Every call
// gets its own copy, Variables included, so a caller that rewrites a variable
// for one run neither leaks into later runs nor races with concurrent ones.
func StaticPrompt(p Prompt) PromptProvider {
	return func(context.Context, *RunContext, *Agent) (*Prompt, error) {
		pp := p
		pp.Variables = maps.Clone(p.Variables)
		return &pp, nil
	}
}
