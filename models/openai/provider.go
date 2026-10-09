package openai

import (
	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/modelkit"
)

// Provider resolves model names to ResponsesModel instances backed by a shared
// OpenAI client. It implements agents.ModelProvider.
type Provider struct {
	client oai.Client
	// defaultModel is used for agents that omit a model name; the SDK ships
	// no built-in default, so unset it makes an empty name a UserError.
	defaultModel string
}

// NewProvider builds a Provider from openai-go request options
// (option.WithAPIKey, option.WithBaseURL, …); with none, OPENAI_API_KEY is
// read. The client's own retries are DISABLED (decisions §5.22);
// option.WithMaxRetries re-enables them.
func NewProvider(opts ...option.RequestOption) *Provider {
	all := append([]option.RequestOption{option.WithMaxRetries(0)}, opts...)
	return &Provider{client: oai.NewClient(all...)}
}

// WithDefaultModel sets the model used when an agent omits a model name. Without
// it, resolving an agent that names no model is a UserError.
func (p *Provider) WithDefaultModel(name string) *Provider {
	p.defaultModel = name
	return p
}

// Capabilities declares this adapter's unsupported request features: none, the
// Responses API being the SDK's native format.
func Capabilities() modelkit.Capabilities {
	return modelkit.Capabilities{}
}

// Model implements agents.ModelProvider.
func (p *Provider) Model(modelName string) (agents.Model, error) {
	if modelName == "" {
		modelName = p.defaultModel
	}
	if modelName == "" {
		return nil, agents.NewUserError("openai: no model specified — set Agent.Model or Provider.WithDefaultModel")
	}
	return NewResponsesModel(modelName, p.client.Responses), nil
}

var _ agents.ModelProvider = (*Provider)(nil)
