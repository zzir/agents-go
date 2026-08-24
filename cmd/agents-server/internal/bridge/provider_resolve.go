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
	// Re-check the reference rule at run time: a scope flip that slipped past
	// the write-time guards must fail the run loudly, never spend a key that
	// became somebody's private credential (spec §5.29).
	if !store.RefVisible(pv.Scope, pv.OwnerID, ac.Scope, ac.OwnerID) {
		return store.Provider{}, fmt.Errorf("agent %q: provider %s is out of the agent's scope — repoint the agent", ac.Name, ac.ProviderID)
	}
	return *pv, nil
}

// resolveProvider builds the agent's model provider: the endpoint its provider
// row names, wrapped with retry and fallback decorators when enabled. It
// returns a nil provider (no error) when no API key is available — the
// run-without-a-provider path — and always returns the normalized providerType.
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
	// Validation forbids a custom base_url with chatgpt_login, so this only
	// fills the default; it stays here as the belt to the validation's braces —
	// the OAuth token never rides to an operator-typed host.
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
	return provider, def.Type, nil
}

type fallbackEntry struct {
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	// Provider selects the backend ("openai" / "anthropic"); empty is openai.
	// The JSON key is provider_type, matching the agent config group's
	// spelling of the same selector.
	Provider string `json:"provider_type"`
}

// fixedModelProvider pins a provider to one model name, ignoring the name the
// run requests. The SDK's FallbackProvider asks every fallback for the SAME
// (primary) model name, so without this a configured fallback model would never
// be used — the fallback would just retry the primary's model name elsewhere.
type fixedModelProvider struct {
	inner agents.ModelProvider
	model string
}

func (f fixedModelProvider) Model(string) (agents.Model, error) {
	return f.inner.Model(f.model)
}

// wrapFallbackProvider chains one fixed-model provider per decoded fallback
// entry behind primary. The entries are decoded up front (DecodeAgentSpec), so
// this is pure construction — callers gate it on len(entries) > 0. An entry
// carries its own credential (api_key) or runs keyless.
func wrapFallbackProvider(primary agents.ModelProvider, entries []fallbackEntry, proxyClient *http.Client) agents.ModelProvider {
	var fallbacks []agents.ModelProvider
	for _, e := range entries {
		fp, err := providers.BuildPlain(e.Provider, e.APIKey, e.BaseURL, proxyClient)
		if err != nil {
			// Unreachable through normal flow — DecodeAgentSpec validates every
			// entry's provider — and an unbuildable entry must not become an
			// OpenAI default, so it is left out of the chain.
			continue
		}
		if e.Model != "" {
			fp = fixedModelProvider{inner: fp, model: e.Model}
		}
		fallbacks = append(fallbacks, fp)
	}
	return agents.NewFallbackProvider(primary, fallbacks...)
}
