// Package anthropic implements the agents Model interface against the
// Anthropic Messages API, using the official anthropic-sdk-go.
//
// The adapter translates in both directions at the model boundary: canonical
// Responses input items become Messages turns, and Messages content blocks /
// stream events come back as canonical items and response.* events (via
// modelkit). Everything outside the adapter — the runner, sessions, run
// state, the server — keeps speaking the canonical format and does not know
// this backend exists.
//
// Request features the Messages API has no equivalent for fail with a
// *agents.UserError rather than being dropped; see Capabilities for the list.
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
	// promptCaching applies the request-level cache_control marker, which
	// caches the request prefix up to its last cacheable block. On by default:
	// an agent loop resends a growing prefix every turn, which is the exact
	// shape prompt caching exists for, and the marker is free when nothing is
	// cacheable. WithPromptCaching(false) opts out.
	promptCaching bool
}

// NewProvider builds a Provider. Pass anthropic-sdk-go request options such as
// option.WithAPIKey or option.WithBaseURL to configure the client. With no
// options, the API key is read from the ANTHROPIC_API_KEY environment variable.
//
// The client's own transport-level retries are DISABLED (anthropic-sdk-go
// defaults to 2), for the same reason models/openai does it: retry policy
// belongs to one layer — agents.NewRetryModel — and stacked layers multiply.
// Pass option.WithMaxRetries explicitly to re-enable the transport layer.
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
