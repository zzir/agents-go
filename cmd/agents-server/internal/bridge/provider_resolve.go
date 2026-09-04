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
	if err := providers.Validate(&pv); err != nil {
		return nil, "", fmt.Errorf("agent %q: %w", ac.Name, err)
	}
	def, err := providers.DefFor(pv.Type)
	if err != nil {
		return nil, "", err // unreachable after validation; fail loud, never default
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
		return nil, def.Type, nil
	}
	baseURL := pv.BaseURL
	// Validation forbids a custom base_url with chatgpt_login; this is the belt
	// to that: the OAuth token never rides to an operator-typed host.
	if chatgptCreds != nil {
		baseURL = providers.ChatGPTBaseURL
	}
	provider := def.Build(apiKey, baseURL, chatgptCreds, proxyClient)
	if ac.Resilience.RetryEnabled {
		provider = agents.NewRetryProvider(provider, spec.RetryPolicy)
	}
	if len(spec.FallbackModels) > 0 {
		provider = wrapFallbackProvider(provider, spec.FallbackModels, proxyClient)
	}
	// Outermost, so every model — fallbacks included — resolves attachment
	// sentinels at the request edge (see attachment_hydrate.go).
	provider = hydrateAttachments(provider, deps.Attachments, func(ctx context.Context) string {
		return deps.Settings.S3Config(ctx).PublicBaseURL
	})
	return provider, def.Type, nil
}

type fallbackEntry struct {
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	// Provider selects the backend ("openai" / "anthropic"); empty is openai.
	// The JSON key is provider_type, as the agent config group spells it.
	Provider string `json:"provider_type"`
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

// wrapFallbackProvider chains one fixed-model provider per fallback entry behind
// primary; an entry carries its own api_key or runs keyless.
func wrapFallbackProvider(primary agents.ModelProvider, entries []fallbackEntry, proxyClient *http.Client) agents.ModelProvider {
	var fallbacks []agents.ModelProvider
	for _, e := range entries {
		fp, err := providers.BuildPlain(e.Provider, e.APIKey, e.BaseURL, proxyClient)
		if err != nil {
			// Unreachable (DecodeAgentSpec validates every entry); an unbuildable
			// entry must not become an OpenAI default, so it is left out.
			continue
		}
		if e.Model != "" {
			fp = fixedModelProvider{inner: fp, model: e.Model}
		}
		fallbacks = append(fallbacks, fp)
	}
	return agents.NewFallbackProvider(primary, fallbacks...)
}
