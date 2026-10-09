// Package anthropic implements the agents Model interface against the
// Anthropic Messages API via anthropic-sdk-go, translating canonical Responses
// items in and canonical items and response.* events out at the model boundary
// (decisions §5.10). Features the API lacks fail with a *agents.UserError.
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
	// promptCaching applies the request-level cache_control marker; on by default.
	promptCaching bool
	// budgetThinking: see WithBudgetThinking.
	budgetThinking bool
	// thinkingBinding: see WithThinkingBinding. On by default.
	thinkingBinding bool
}

// NewProvider builds a Provider from anthropic-sdk-go request options
// (option.WithAPIKey, option.WithBaseURL, …); with none, ANTHROPIC_API_KEY is
// read. The client's own retries are DISABLED (decisions §5.22);
// option.WithMaxRetries re-enables them.
func NewProvider(opts ...option.RequestOption) *Provider {
	all := append([]option.RequestOption{option.WithMaxRetries(0)}, opts...)
	return &Provider{client: ant.NewClient(all...), promptCaching: true, thinkingBinding: true}
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

// WithBudgetThinking sends a reasoning effort as a thinking token budget
// instead of adaptive thinking: the form models without adaptive thinking take
// (Claude Haiku 4.5 and older) — decisions §5.76.
func (p *Provider) WithBudgetThinking(enabled bool) *Provider {
	p.budgetThinking = enabled
	return p
}

// WithThinkingBinding toggles asking the API to drop a replayed thinking block
// bound to a prefix that has since changed, instead of failing the request
// (decisions §5.77). Turn it off for an endpoint that rejects the beta header.
func (p *Provider) WithThinkingBinding(enabled bool) *Provider {
	p.thinkingBinding = enabled
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
	return &MessagesModel{model: modelName, client: p.client.Messages, promptCaching: p.promptCaching,
		budgetThinking: p.budgetThinking, thinkingBinding: p.thinkingBinding}, nil
}

var _ agents.ModelProvider = (*Provider)(nil)
