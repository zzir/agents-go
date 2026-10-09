package agents

import (
	"context"
	"iter"
)

// ModelRequest bundles the parameters for a single model call; zero values
// are the defaults.
type ModelRequest struct {
	// SystemInstructions is the system prompt, if any.
	SystemInstructions string
	// Input is the conversation history in OpenAI Responses input format.
	Input []InputItem
	// Settings holds the model configuration (temperature, etc).
	Settings *ModelSettings
	// Tools are the tools available to the model.
	Tools []*Tool
	// OutputSchema describes the structured output, or nil for plain text.
	OutputSchema OutputSchema
	// Handoffs are the handoffs available to the model.
	Handoffs []Handoff
	// PreviousResponseID chains to a prior response (Responses API only).
	PreviousResponseID string
	// ConversationID references a stored conversation, if any.
	ConversationID string
}

// Model is the interface for calling an LLM; implementations live in the
// models subpackages.
type Model interface {
	// Respond performs a single, non-streaming model call.
	Respond(ctx context.Context, req ModelRequest) (*ModelResponse, error)

	// StreamResponse performs a streaming model call, yielding raw Responses API
	// stream events; a non-nil error is terminal.
	StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*ResponseStreamEvent, error]
}

// ModelProvider resolves an agent's model name to a Model.
type ModelProvider interface {
	// Model returns the model for the given name; empty selects the provider's default.
	Model(modelName string) (Model, error)
}
