package agents

import (
	"context"
	"iter"
)

// ModelRequest bundles the parameters for a single model call. It replaces the
// long positional argument list of the Python SDK's Model.get_response with a
// struct whose zero values are sensible defaults.
type ModelRequest struct {
	// SystemInstructions is the system prompt, if any.
	SystemInstructions string
	// Prompt is the OpenAI stored-prompt configuration, if the agent set one.
	Prompt *Prompt
	// Input is the conversation history in OpenAI Responses input format.
	Input []TResponseInputItem
	// Settings holds the model configuration (temperature, etc).
	Settings *ModelSettings
	// Tools are the tools available to the model.
	Tools []Tool
	// OutputSchema describes the structured output, or nil for plain text.
	OutputSchema OutputSchema
	// Handoffs are the handoffs available to the model.
	Handoffs []Handoff
	// PreviousResponseID chains to a prior response (Responses API only).
	PreviousResponseID string
	// ConversationID references a stored conversation, if any.
	ConversationID string
}

// Model is the interface for calling an LLM. Implementations live in provider
// subpackages (e.g. openai).
type Model interface {
	// GetResponse performs a single, non-streaming model call.
	GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error)

	// StreamResponse performs a streaming model call, yielding raw Responses API
	// stream events. The second iterator value carries any terminal error; once
	// a non-nil error is yielded, iteration stops.
	StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*TResponseStreamEvent, error]
}

// ModelProvider looks up Models by name. The runner uses it to resolve an
// agent's model when the agent specifies a name rather than a concrete Model.
type ModelProvider interface {
	// GetModel returns the model for the given name. An empty name selects the
	// provider's default model.
	GetModel(modelName string) (Model, error)
}
