package bridge

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	antoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	anthropicProvider "github.com/zzir/agents-go/models/anthropic"
	"github.com/zzir/agents-go/models/modelkit"
	openaiProvider "github.com/zzir/agents-go/models/openai"
)

// The provider_type values an agent config, fallback entry or provider route
// may select. Empty means openai — the value predates the field.
const (
	ProviderTypeOpenAI    = "openai"
	ProviderTypeAnthropic = "anthropic"
)

// authModeChatGPTLogin is the one auth mode beyond a plain API key, and it is
// OpenAI-only: its middleware rewrites Responses-shaped request bodies.
const authModeChatGPTLogin = "chatgpt_login"

// providerDef is one backend the server can build providers for. It is an
// INTERNAL table, not a plugin API: everything provider-selection touches —
// validation, construction, the global key setting, auth modes, capability
// metadata — derives from this slice, so adding a backend is one entry here
// (plus its SDK module and a row in the frontend's PROVIDERS table) instead
// of a hunt across bridge, handlers and docs. Being internal also means its shape may
// be reworked freely when a third backend's auth or credential model does not
// fit the current fields.
type providerDef struct {
	// Type is the provider_type wire value.
	Type string
	// SettingKey is the global fallback API-key setting ("openai_api_key", …),
	// used when an agent or fallback entry carries no key of its own. Also
	// derived into handler.secretSettingKeys, so the key is masked on read.
	SettingKey string
	// AuthModes lists auth_mode values beyond "" (API key) this backend
	// accepts. Validation rejects any other combination.
	AuthModes []string
	// Build constructs the provider. creds is nil except for a
	// chatgpt_login-authenticated agent (which validation limits to backends
	// listing that auth mode); backends without that auth mode ignore it. The
	// routes / fallback-entries path always passes nil.
	Build func(apiKey, baseURL string, creds *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider
	// Capabilities is the adapter's own unsupported-feature declaration,
	// served to config UIs via ProviderTypes.
	Capabilities modelkit.Capabilities
}

var providerDefs = []providerDef{
	{
		Type:         ProviderTypeOpenAI,
		SettingKey:   "openai_api_key",
		AuthModes:    []string{authModeChatGPTLogin},
		Build:        newOpenAIModelProvider,
		Capabilities: openaiProvider.Capabilities(),
	},
	{
		Type:       ProviderTypeAnthropic,
		SettingKey: "anthropic_api_key",
		Build: func(apiKey, baseURL string, _ *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider {
			return newAnthropicModelProvider(apiKey, baseURL, proxyClient)
		},
		Capabilities: anthropicProvider.Capabilities(),
	},
}

func newOpenAIModelProvider(apiKey, baseURL string, creds *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if creds != nil {
		opts = append(opts, option.WithMiddleware(newChatGPTMiddleware(creds.AccountID)))
	}
	if proxyClient != nil {
		opts = append(opts, option.WithHTTPClient(proxyClient))
	}
	p := openaiProvider.NewProvider(opts...)
	if creds != nil {
		// The Codex backend rejects non-streaming requests (400), so blocking
		// Respond callers — title gen, compaction summaries, playground —
		// are served by an internal stream instead.
		return agents.NewStreamOnlyProvider(p)
	}
	return p
}

func newAnthropicModelProvider(apiKey, baseURL string, proxyClient *http.Client) agents.ModelProvider {
	var opts []antoption.RequestOption
	if apiKey != "" {
		opts = append(opts, antoption.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, antoption.WithBaseURL(baseURL))
	}
	if proxyClient != nil {
		opts = append(opts, antoption.WithHTTPClient(proxyClient))
	}
	return anthropicProvider.NewProvider(opts...)
}

func normalizeProviderType(t string) string {
	if t == "" {
		return ProviderTypeOpenAI
	}
	return t
}

// NormalizeProviderType maps the empty provider selector to its meaning
// ("openai", which predates the field). Exported for the handlers' secret
// round-tripping: whether a masked key may be restored depends on whether
// the PROVIDER changed, and that comparison must treat "" and "openai" as
// the same backend.
func NormalizeProviderType(t string) string { return normalizeProviderType(t) }

// providerDefFor resolves a provider selector to its definition. The error
// names the valid set, and every construction path handles it rather than
// defaulting — a value outside the table must never silently run on OpenAI.
func providerDefFor(t string) (providerDef, error) {
	t = normalizeProviderType(t)
	for _, d := range providerDefs {
		if d.Type == t {
			return d, nil
		}
	}
	types := make([]string, len(providerDefs))
	for i, d := range providerDefs {
		types[i] = d.Type
	}
	return providerDef{}, fmt.Errorf("unknown provider %q (valid: %s)", t, strings.Join(types, ", "))
}

// buildPlainProvider is the lookup+Build pairing for routes and fallback
// entries.
func buildPlainProvider(providerType, apiKey, baseURL string, proxyClient *http.Client) (agents.ModelProvider, error) {
	def, err := providerDefFor(providerType)
	if err != nil {
		return nil, err
	}
	return def.Build(apiKey, baseURL, nil, proxyClient), nil
}

// ValidateProviderType rejects a provider selector outside the registry. It
// backs both save-time validation and build time, so a value that sneaks past
// one still fails the other loudly instead of silently running on OpenAI.
func ValidateProviderType(t string) error {
	_, err := providerDefFor(t)
	return err
}

// ValidateProviderSelection checks the provider group's cross-field
// constraints: a registered provider_type, and an auth_mode the backend
// actually offers.
func ValidateProviderSelection(ac *store.AgentConfig) error {
	def, err := providerDefFor(ac.Provider.ProviderType)
	if err != nil {
		return fmt.Errorf("provider_type: %w", err)
	}
	if mode := ac.Provider.AuthMode; mode != "" && !slices.Contains(def.AuthModes, mode) {
		return fmt.Errorf("auth_mode %q is not available on the %s provider — use an API key or switch provider_type", mode, def.Type)
	}
	return nil
}

// ProviderTypeInfo is the machine-readable slice of a provider definition
// served to config UIs: which backends exist, what auth they offer, which
// request features fail loudly on them, and where their global fallback key
// lives. Display copy (labels, placeholders) deliberately stays in the
// frontend — this is facts, not wording.
type ProviderTypeInfo struct {
	Type string `json:"type"`
	// AuthModes and Unsupported serialize as [] rather than being omitted:
	// a client caching these facts must be able to tell "this backend has
	// none" apart from "not fetched yet" — omitempty would make an emptied
	// list look like missing data and let a stale local fallback win.
	AuthModes   []string `json:"auth_modes"`
	Unsupported []string `json:"unsupported"`
	SettingKey  string   `json:"setting_key"`
}

// ProviderTypes lists the registered backends. The order is the registry's
// (openai first), which UIs may rely on for a default. Slices are copies —
// a caller mutating its result must not reach the registry.
func ProviderTypes() []ProviderTypeInfo {
	out := make([]ProviderTypeInfo, len(providerDefs))
	for i, d := range providerDefs {
		info := ProviderTypeInfo{
			Type:        d.Type,
			AuthModes:   append([]string{}, d.AuthModes...),
			Unsupported: make([]string, 0, len(d.Capabilities.Unsupported)),
			SettingKey:  d.SettingKey,
		}
		for _, f := range d.Capabilities.Unsupported {
			info.Unsupported = append(info.Unsupported, string(f))
		}
		out[i] = info
	}
	return out
}
