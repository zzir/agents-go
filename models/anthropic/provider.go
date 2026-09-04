// Package anthropic implements the agents Model interface against the
// Anthropic Messages API, using the official anthropic-sdk-go. The adapter
// translates in both directions at the model boundary — canonical Responses
// items in, canonical items and response.* events out (via modelkit) — so
// nothing outside it knows this backend exists (decisions §5.10). Features the
// API lacks fail with a *agents.UserError; see Capabilities.
package anthropic

import (
	ant "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/zzir/agents-go/agents"
)

// Provider resolves model names to MessagesModel instances backed by a shared
// Anthropic client. It implements agents.ModelProvider.
type Provider struct {
	client       ant.Client
	defaultModel string
	// promptCaching applies the request-level cache_control marker: on by
	// default, since an agent loop resends a growing prefix every turn.
	promptCaching bool
}

// NewProvider builds a Provider. Pass anthropic-sdk-go request options such as
// option.WithAPIKey or option.WithBaseURL to configure the client. With no
// options, the API key is read from the ANTHROPIC_API_KEY environment variable.
// The client's own transport-level retries are DISABLED, as in models/openai
// (decisions §5.22); pass option.WithMaxRetries explicitly to re-enable them.
func NewProvider(opts ...option.RequestOption) *Provider {
	all := append([]option.RequestOption{option.WithMaxRetries(0)}, opts...)
	return &Provider{client: ant.NewClient(all...), promptCaching: true}
}

// WithDefaultModel sets the model used when an agent omits a model name.
// Without it, resolving an agent that names no model is a UserError.
func (p *Provider) WithDefaultModel(name string) *Provider {
	p.defaultModel = name
	return p
}

// WithPromptCaching toggles the automatic request-prefix cache_control marker.
func (p *Provider) WithPromptCaching(enabled bool) *Provider {
	p.promptCaching = enabled
	return p
}

// Model implements agents.ModelProvider.
func (p *Provider) Model(modelName string) (agents.Model, error) {
	if modelName == "" {
		modelName = p.defaultModel
	}
	if modelName == "" {
		return nil, agents.NewUserError("anthropic: no model specified — set Agent.Model or Provider.WithDefaultModel")
	}
	return &MessagesModel{model: modelName, client: p.client.Messages, promptCaching: p.promptCaching}, nil
}

var _ agents.ModelProvider = (*Provider)(nil)
