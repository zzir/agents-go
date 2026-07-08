package openai

import (
	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
)

// Provider resolves model names to ResponsesModel instances backed by a shared
// OpenAI client. It implements agents.ModelProvider.
type Provider struct {
	client oai.Client
	// defaultModel is used for agents that omit a model name. It is unset by
	// default: unlike the Python SDK, this port ships no built-in default model,
	// so a model must be named per agent (Agent.Model) or configured here via
	// WithDefaultModel. Resolving an empty name with no default is a UserError.
	defaultModel string
}

// NewProvider builds a Provider. Pass openai-go request options such as
// option.WithAPIKey or option.WithBaseURL to configure the client. With no
// options, the API key is read from the OPENAI_API_KEY environment variable.
func NewProvider(opts ...option.RequestOption) *Provider {
	return &Provider{client: oai.NewClient(opts...)}
}

// WithDefaultModel sets the model used when an agent omits a model name. Without
// it, resolving an agent that names no model is a UserError.
func (p *Provider) WithDefaultModel(name string) *Provider {
	p.defaultModel = name
	return p
}

// GetModel implements agents.ModelProvider.
func (p *Provider) GetModel(modelName string) (agents.Model, error) {
	if modelName == "" {
		modelName = p.defaultModel
	}
	if modelName == "" {
		return nil, &agents.UserError{AgentsError: agents.AgentsError{
			Message: "openai: no model specified — set Agent.Model or Provider.WithDefaultModel",
		}}
	}
	return NewResponsesModel(modelName, p.client.Responses), nil
}

var _ agents.ModelProvider = (*Provider)(nil)
