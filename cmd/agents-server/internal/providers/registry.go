// Package providers is the registry of model-provider backends — their types,
// auth modes and builders — and the ChatGPT login one of them offers.
package providers

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	antoption "github.com/anthropics/anthropic-sdk-go/option"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	anthropicProvider "github.com/zzir/agents-go/models/anthropic"
	"github.com/zzir/agents-go/models/modelkit"
	openaiProvider "github.com/zzir/agents-go/models/openai"
)

// The provider_type values a provider row (or a legacy fallback entry) may
// select; empty means openai.
const (
	TypeOpenAI    = "openai"
	TypeAnthropic = "anthropic"
)

// AuthModeChatGPTLogin is the one auth mode beyond a plain API key, and it is
// OpenAI-only: its middleware rewrites Responses-shaped request bodies.
const AuthModeChatGPTLogin = store.AuthModeChatGPTLogin

// Def is one backend the server can build providers for: validation,
// construction, auth modes and capability metadata all derive from this
// table. A new backend is one entry, its SDK module and a frontend PROVIDERS row.
type Def struct {
	// Type is the provider_type wire value.
	Type string
	// AuthModes lists auth_mode values beyond "" (API key) this backend
	// accepts. Validation rejects any other combination.
	AuthModes []string
	// Build constructs the provider. creds is set only for a chatgpt_login
	// provider; the fallback-entries path always passes nil.
	Build func(apiKey, baseURL string, creds *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider
	// Capabilities is the adapter's own unsupported-feature declaration,
	// served to config UIs via Types.
	Capabilities modelkit.Capabilities
	// ListModels asks the backend which models the key may use — a live
	// answer from the provider, never a table of this project's.
	ListModels func(ctx context.Context, apiKey, baseURL string, hc *http.Client) ([]ModelInfo, error)
}

// ModelInfo is one model a provider lists; what a backend does not report stays zero.
type ModelInfo struct {
	ID string `json:"id"`
	// DisplayName is the provider's human-readable name, when it gives one.
	DisplayName string `json:"display_name,omitempty"`
	// ContextWindow is the model's input context in tokens.
	ContextWindow int64 `json:"context_window,omitempty"`
	// MaxOutputTokens is the ceiling of max_tokens for the model.
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	// ThinkingTypes lists the thinking forms the model takes ("adaptive", "enabled").
	ThinkingTypes []string `json:"thinking_types,omitempty"`
}

var providerDefs = []Def{
	{
		Type:         TypeOpenAI,
		AuthModes:    []string{AuthModeChatGPTLogin},
		Build:        newOpenAIModelProvider,
		Capabilities: openaiProvider.Capabilities(),
		ListModels:   listOpenAIModels,
	},
	{
		Type: TypeAnthropic,
		Build: func(apiKey, baseURL string, _ *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider {
			return newAnthropicModelProvider(apiKey, baseURL, proxyClient)
		},
		Capabilities: anthropicProvider.Capabilities(),
		ListModels:   listAnthropicModels,
	},
}

func openaiOptions(apiKey, baseURL string, hc *http.Client) []option.RequestOption {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	return opts
}

func anthropicOptions(apiKey, baseURL string, hc *http.Client) []antoption.RequestOption {
	var opts []antoption.RequestOption
	if apiKey != "" {
		opts = append(opts, antoption.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, antoption.WithBaseURL(baseURL))
	}
	if hc != nil {
		opts = append(opts, antoption.WithHTTPClient(hc))
	}
	return opts
}

// listOpenAIModels reads GET /models; the Responses-shaped listing names
// models only, so the window and output ceiling stay unknown.
func listOpenAIModels(ctx context.Context, apiKey, baseURL string, hc *http.Client) ([]ModelInfo, error) {
	client := openaisdk.NewClient(append(openaiOptions(apiKey, baseURL, hc), option.WithMaxRetries(0))...)
	var out []ModelInfo
	it := client.Models.ListAutoPaging(ctx)
	for it.Next() {
		out = append(out, ModelInfo{ID: it.Current().ID})
	}
	return out, it.Err()
}

// listAnthropicModels reads GET /v1/models, which carries each model's
// window, output ceiling and the thinking forms it takes.
func listAnthropicModels(ctx context.Context, apiKey, baseURL string, hc *http.Client) ([]ModelInfo, error) {
	client := anthropicsdk.NewClient(append(anthropicOptions(apiKey, baseURL, hc), antoption.WithMaxRetries(0))...)
	var out []ModelInfo
	it := client.Models.ListAutoPaging(ctx, anthropicsdk.ModelListParams{})
	for it.Next() {
		m := it.Current()
		info := ModelInfo{ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.MaxInputTokens, MaxOutputTokens: m.MaxTokens}
		if m.Capabilities.Thinking.Types.Adaptive.Supported {
			info.ThinkingTypes = append(info.ThinkingTypes, "adaptive")
		}
		if m.Capabilities.Thinking.Types.Enabled.Supported {
			info.ThinkingTypes = append(info.ThinkingTypes, "enabled")
		}
		out = append(out, info)
	}
	return out, it.Err()
}

func newOpenAIModelProvider(apiKey, baseURL string, creds *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider {
	opts := openaiOptions(apiKey, baseURL, proxyClient)
	if creds != nil {
		opts = append(opts, option.WithMiddleware(newChatGPTMiddleware(creds.AccountID)))
	}
	p := openaiProvider.NewProvider(opts...)
	if creds != nil {
		// The Codex backend accepts only streaming requests — decisions §5.15.
		return agents.NewStreamOnlyProvider(p)
	}
	return p
}

func newAnthropicModelProvider(apiKey, baseURL string, proxyClient *http.Client) agents.ModelProvider {
	return anthropicProvider.NewProvider(anthropicOptions(apiKey, baseURL, proxyClient)...)
}

// ThinkingModeBudget is the behavior.thinking_mode value that sends an
// Anthropic backend's reasoning effort as a thinking token budget.
const ThinkingModeBudget = "budget"

// ApplyThinking sets how an Anthropic provider sends the reasoning effort and
// whether it asks for mismatched thinking to be dropped; any other provider is
// returned as it came.
func ApplyThinking(p agents.ModelProvider, mode string, binding bool) agents.ModelProvider {
	if ap, ok := p.(*anthropicProvider.Provider); ok {
		ap.WithBudgetThinking(mode == ThinkingModeBudget).WithThinkingBinding(binding)
	}
	return p
}

func normalizeType(t string) string {
	if t == "" {
		return TypeOpenAI
	}
	return t
}

// NormalizeType maps the empty provider selector to its meaning ("openai"),
// so a comparison of two rows' backends treats "" and "openai" as one.
func NormalizeType(t string) string { return normalizeType(t) }

// NormalizeBaseURL canonicalizes a base_url for comparing two rows' endpoints:
// whitespace and the trailing slash only, never anything that could equate two hosts.
func NormalizeBaseURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// SameEndpoint reports whether two (type, base_url) pairs reach the same backend.
func SameEndpoint(typeA, baseA, typeB, baseB string) bool {
	return normalizeType(typeA) == normalizeType(typeB) && NormalizeBaseURL(baseA) == NormalizeBaseURL(baseB)
}

// DefFor resolves a provider selector to its definition; the error names the
// valid set, and no construction path defaults past it.
func DefFor(t string) (Def, error) {
	t = normalizeType(t)
	for _, d := range providerDefs {
		if d.Type == t {
			return d, nil
		}
	}
	types := make([]string, len(providerDefs))
	for i, d := range providerDefs {
		types[i] = d.Type
	}
	return Def{}, fmt.Errorf("unknown provider %q (valid: %s)", t, strings.Join(types, ", "))
}

// BuildPlain is the lookup+Build pairing for fallback entries.
func BuildPlain(providerType, apiKey, baseURL string, proxyClient *http.Client) (agents.ModelProvider, error) {
	def, err := DefFor(providerType)
	if err != nil {
		return nil, err
	}
	return def.Build(apiKey, baseURL, nil, proxyClient), nil
}

// ValidateType rejects a provider selector outside the registry; both save
// time and build time run it.
func ValidateType(t string) error {
	_, err := DefFor(t)
	return err
}

// Validate checks a provider row's cross-field constraints: a registered
// type, and an auth_mode the backend offers. The zero value (the keyless
// built-in default) passes.
func Validate(pv *store.Provider) error {
	def, err := DefFor(pv.Type)
	if err != nil {
		return fmt.Errorf("type: %w", err)
	}
	if mode := pv.AuthMode; mode != "" && !slices.Contains(def.AuthModes, mode) {
		return fmt.Errorf("auth_mode %q is not available on the %s provider — use an API key or switch the type", mode, def.Type)
	}
	// The OAuth access token is the bearer: it goes to ChatGPT only, so no
	// custom base URL.
	if pv.AuthMode == AuthModeChatGPTLogin && pv.BaseURL != "" {
		return fmt.Errorf("base_url cannot be set with chatgpt_login: the OAuth token is only ever sent to ChatGPT")
	}
	return nil
}

// TypeInfo is the slice of a provider definition served to config UIs:
// which backends exist, what auth they offer, and which request features
// fail on them. Display copy stays in the frontend.
type TypeInfo struct {
	Type string `json:"type"`
	// AuthModes and Unsupported serialize as [] rather than being omitted:
	// a client caching these facts must be able to tell "this backend has
	// none" apart from "not fetched yet" — omitempty would make an emptied
	// list look like missing data and let a stale local fallback win.
	AuthModes   []string `json:"auth_modes"`
	Unsupported []string `json:"unsupported"`
}

// Types lists the registered backends in registry order (openai first,
// which UIs may take as the default); the slices are copies.
func Types() []TypeInfo {
	out := make([]TypeInfo, len(providerDefs))
	for i, d := range providerDefs {
		info := TypeInfo{
			Type:        d.Type,
			AuthModes:   append([]string{}, d.AuthModes...),
			Unsupported: make([]string, 0, len(d.Capabilities.Unsupported)),
		}
		for _, f := range d.Capabilities.Unsupported {
			info.Unsupported = append(info.Unsupported, string(f))
		}
		out[i] = info
	}
	return out
}
