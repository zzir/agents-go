package bridge

import (
	"context"
	"fmt"
	"net/http"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// How an agent config reaches its model: the provider row it names, the key
// that unlocks it, and the retry and fallback decorators around it.

// AgentProvider loads the endpoint an agent reaches its model through. An
// empty provider_id yields the ZERO provider — the openai backend with no
// credential, so the run fails its pre-flight until the agent names a
// provider that carries one. An agent that NAMES a provider on a host with
// no provider store is an error, never a silent fall-through to the default
// — that would run it on the wrong backend with the wrong key.
func AgentProvider(ctx context.Context, deps *AgentDeps, ac *store.AgentConfig) (store.Provider, error) {
	if ac.ProviderID == "" {
		return store.Provider{}, nil
	}
	if deps.Providers == nil {
		return store.Provider{}, fmt.Errorf("agent %q names provider %s but no provider store is wired", ac.Name, ac.ProviderID)
	}
	pv, err := deps.Providers.Get(ctx, ac.ProviderID)
	if err != nil {
		return store.Provider{}, fmt.Errorf("agent %q: provider %s: %w", ac.Name, ac.ProviderID, err)
	}
	// Re-checked at run time: a scope flip past the write-time guards must fail
	// loudly, never spend a now-private key (decisions §5.29).
	if !store.RefVisible(pv.Scope, pv.OwnerID, ac.Scope, ac.OwnerID) {
		return store.Provider{}, fmt.Errorf("agent %q: provider %s is out of the agent's scope — repoint the agent", ac.Name, ac.ProviderID)
	}
	return *pv, nil
}

// resolveProvider builds the agent's model provider with retry and fallback
// decorators; nil (no error) when no API key is available.
func resolveProvider(ctx context.Context, deps *AgentDeps, ac *store.AgentConfig, spec *AgentSpec, proxyClient *http.Client) (agents.ModelProvider, string, error) {
	pv, err := AgentProvider(ctx, deps, ac)
	if err != nil {
		return nil, "", err
	}
	provider, def, err := buildProvider(ctx, deps, ac, pv, proxyClient)
	if err != nil {
		return nil, "", err
	}
	if provider == nil {
		return nil, def.Type, nil
	}
	if ac.Resilience.RetryEnabled {
		provider = agents.NewRetryProvider(provider, spec.RetryPolicy)
	}
	if len(spec.FallbackModels) > 0 {
		fallbacks, err := fallbackProviders(ctx, deps, ac, spec.FallbackModels, proxyClient)
		if err != nil {
			return nil, "", err
		}
		provider = agents.NewFallbackProvider(provider, fallbacks...)
	}
	// Outermost, so every model — fallbacks included — resolves attachment
	// sentinels at the request edge (see attachment_hydrate.go).
	provider = hydrateAttachments(provider, deps.Attachments, func(ctx context.Context) string {
		return deps.Settings.S3Config(ctx).PublicBaseURL
	})
	return provider, def.Type, nil
}

// buildProvider turns a provider row into a model provider; nil (no error)
// when the row reaches no credential.
func buildProvider(ctx context.Context, deps *AgentDeps, ac *store.AgentConfig, pv store.Provider, proxyClient *http.Client) (agents.ModelProvider, providers.Def, error) {
	if err := providers.Validate(&pv); err != nil {
		return nil, providers.Def{}, fmt.Errorf("agent %q: %w", ac.Name, err)
	}
	def, err := providers.DefFor(pv.Type)
	if err != nil {
		return nil, providers.Def{}, err // unreachable after validation; fail loud, never default
	}
	apiKey := pv.APIKey
	var chatgptCreds *providers.ChatGPTCredentials
	if pv.AuthMode == providers.AuthModeChatGPTLogin && deps.ChatGPTOAuth != nil {
		if creds, err := deps.ChatGPTOAuth.GetCredentials(ctx, pv.ID); err == nil {
			apiKey = creds.AccessToken
			chatgptCreds = creds
		} else {
			logging.Ctx(ctx).Warn("ChatGPT OAuth token unavailable, falling back to api_key", "error", err)
		}
	}
	if apiKey == "" {
		return nil, def, nil
	}
	baseURL := pv.BaseURL
	// Validation forbids a custom base_url with chatgpt_login; this is the belt
	// to that: the OAuth token never rides to an operator-typed host.
	if chatgptCreds != nil {
		baseURL = providers.ChatGPTBaseURL
	}
	return def.Build(apiKey, baseURL, chatgptCreds, proxyClient), def, nil
}

// fallbackProviders resolves each fallback entry to a keyed provider the agent
// may reference, pinned to the entry's model; an entry that resolves to no
// such provider fails the build — decisions §5.69.
func fallbackProviders(ctx context.Context, deps *AgentDeps, ac *store.AgentConfig, entries []store.FallbackModel, proxyClient *http.Client) ([]agents.ModelProvider, error) {
	if deps.Providers == nil {
		return nil, fmt.Errorf("agent %q names fallback providers but no provider store is wired", ac.Name)
	}
	fallbacks := make([]agents.ModelProvider, 0, len(entries))
	for i, e := range entries {
		pv, err := fallbackProvider(ctx, deps, ac, i, e)
		if err != nil {
			return nil, err
		}
		fp, _, err := buildProvider(ctx, deps, ac, pv, proxyClient)
		if err != nil {
			return nil, fmt.Errorf("fallback_models[%d]: %w", i, err)
		}
		if fp == nil {
			return nil, fmt.Errorf("agent %q: fallback_models[%d]: provider %q reaches no API key", ac.Name, i, pv.Name)
		}
		if e.Model != "" {
			fp = fixedModelProvider{inner: fp, model: e.Model}
		}
		fallbacks = append(fallbacks, fp)
	}
	return fallbacks, nil
}

// fallbackProvider loads the entry's provider row: by id, re-checking the
// reference rule like the primary; an entry from before provider_id names an
// endpoint instead, and resolves to the first provider the agent may reference at it.
func fallbackProvider(ctx context.Context, deps *AgentDeps, ac *store.AgentConfig, i int, e store.FallbackModel) (store.Provider, error) {
	if e.ProviderID != "" {
		pv, err := deps.Providers.Get(ctx, e.ProviderID)
		if err != nil {
			return store.Provider{}, fmt.Errorf("agent %q: fallback_models[%d]: provider %s: %w", ac.Name, i, e.ProviderID, err)
		}
		if !store.RefVisible(pv.Scope, pv.OwnerID, ac.Scope, ac.OwnerID) {
			return store.Provider{}, fmt.Errorf("agent %q: fallback_models[%d]: provider %s is out of the agent's scope — repoint the entry", ac.Name, i, e.ProviderID)
		}
		return *pv, nil
	}
	rows, err := deps.Providers.List(ctx)
	if err != nil {
		return store.Provider{}, fmt.Errorf("agent %q: fallback_models[%d]: %w", ac.Name, i, err)
	}
	for _, pv := range rows {
		if providers.SameEndpoint(pv.Type, pv.BaseURL, e.ProviderType, e.BaseURL) && store.RefVisible(pv.Scope, pv.OwnerID, ac.Scope, ac.OwnerID) {
			logging.Ctx(ctx).Warn("fallback entry names an endpoint, not a provider; re-save the agent with provider_id",
				"agent", ac.Name, "index", i, "provider", pv.Name)
			return pv, nil
		}
	}
	return store.Provider{}, fmt.Errorf("agent %q: fallback_models[%d]: no provider at %s %s the agent may reference — add one under Providers and re-save the entry with provider_id",
		ac.Name, i, providers.NormalizeType(e.ProviderType), e.BaseURL)
}

// fixedModelProvider pins a provider to one model name: FallbackProvider asks
// every fallback for the PRIMARY's model name, so a configured one needs this.
type fixedModelProvider struct {
	inner agents.ModelProvider
	model string
}

func (f fixedModelProvider) Model(string) (agents.Model, error) {
	return f.inner.Model(f.model)
}
