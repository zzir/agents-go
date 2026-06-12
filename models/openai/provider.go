package openai

import (
	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
)

// DefaultModel is used when an agent does not specify a model name.
const DefaultModel = "gpt-4o"

// Provider resolves model names to ResponsesModel instances backed by a shared
// OpenAI client. It implements agents.ModelProvider.
type Provider struct {
	client       oai.Client
	defaultModel string
}

// NewProvider builds a Provider. Pass openai-go request options such as
// option.WithAPIKey or option.WithBaseURL to configure the client. With no
// options, the API key is read from the OPENAI_API_KEY environment variable.
func NewProvider(opts ...option.RequestOption) *Provider {
	return &Provider{
		client:       oai.NewClient(opts...),
		defaultModel: DefaultModel,
	}
}

// WithDefaultModel sets the model used when an agent omits a model name.
func (p *Provider) WithDefaultModel(name string) *Provider {
	p.defaultModel = name
	return p
}

// GetModel implements agents.ModelProvider.
func (p *Provider) GetModel(modelName string) (agents.Model, error) {
	if modelName == "" {
		modelName = p.defaultModel
	}
	return NewResponsesModel(modelName, p.client.Responses), nil
}

var _ agents.ModelProvider = (*Provider)(nil)
