// Package providers is the registry of model-provider backends — their types,
// auth modes and builders — and the ChatGPT login one of them offers.
package providers

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
	TypeOpenAI    = "openai"
	TypeAnthropic = "anthropic"
)

// AuthModeChatGPTLogin is the one auth mode beyond a plain API key, and it is
// OpenAI-only: its middleware rewrites Responses-shaped request bodies.
const AuthModeChatGPTLogin = store.AuthModeChatGPTLogin

// Def is one backend the server can build providers for. It is an
// INTERNAL table, not a plugin API: everything provider-selection touches —
// validation, construction, the global key setting, auth modes, capability
// metadata — derives from this slice, so adding a backend is one entry here
// (plus its SDK module and a row in the frontend's PROVIDERS table) instead
// of a hunt across bridge, handlers and docs. Being internal also means its shape may
// be reworked freely when a third backend's auth or credential model does not
// fit the current fields.
type Def struct {
	// Type is the provider_type wire value.
	Type string
	// AuthModes lists auth_mode values beyond "" (API key) this backend
	// accepts. Validation rejects any other combination.
	AuthModes []string
	// Build constructs the provider. creds is nil except for a
	// chatgpt_login-authenticated agent (which validation limits to backends
	// listing that auth mode); backends without that auth mode ignore it. The
	// routes / fallback-entries path always passes nil.
	Build func(apiKey, baseURL string, creds *ChatGPTCredentials, proxyClient *http.Client) agents.ModelProvider
	// Capabilities is the adapter's own unsupported-feature declaration,
	// served to config UIs via Types.
	Capabilities modelkit.Capabilities
}

var providerDefs = []Def{
	{
		Type:         TypeOpenAI,
		AuthModes:    []string{AuthModeChatGPTLogin},
		Build:        newOpenAIModelProvider,
		Capabilities: openaiProvider.Capabilities(),
	},
	{
		Type: TypeAnthropic,
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

func normalizeType(t string) string {
	if t == "" {
		return TypeOpenAI
	}
	return t
}

// NormalizeType maps the empty provider selector to its meaning
// ("openai", which predates the field). Exported for the handlers' secret
// round-tripping: whether a masked key may be restored depends on whether
// the PROVIDER changed, and that comparison must treat "" and "openai" as
// the same backend.
func NormalizeType(t string) string { return normalizeType(t) }

// DefFor resolves a provider selector to its definition. The error
// names the valid set, and every construction path handles it rather than
// defaulting — a value outside the table must never silently run on OpenAI.
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

// BuildPlain is the lookup+Build pairing for routes and fallback
// entries.
func BuildPlain(providerType, apiKey, baseURL string, proxyClient *http.Client) (agents.ModelProvider, error) {
	def, err := DefFor(providerType)
	if err != nil {
		return nil, err
	}
	return def.Build(apiKey, baseURL, nil, proxyClient), nil
}

// ValidateType rejects a provider selector outside the registry. It
// backs both save-time validation and build time, so a value that sneaks past
// one still fails the other loudly instead of silently running on OpenAI.
func ValidateType(t string) error {
	_, err := DefFor(t)
	return err
}

// Validate checks a provider row's cross-field constraints: a
// registered type, and an auth_mode the backend actually offers. The zero
// value passes — it is the built-in default (openai on the global key).
func Validate(pv *store.Provider) error {
	def, err := DefFor(pv.Type)
	if err != nil {
		return fmt.Errorf("type: %w", err)
	}
	if mode := pv.AuthMode; mode != "" && !slices.Contains(def.AuthModes, mode) {
		return fmt.Errorf("auth_mode %q is not available on the %s provider — use an API key or switch the type", mode, def.Type)
	}
	// A ChatGPT-login provider sends the account's OAuth access token as its
	// bearer. Pointed at a custom base URL, that token would be handed to
	// whatever host it names — so the endpoint is fixed, not configurable.
	if pv.AuthMode == AuthModeChatGPTLogin && pv.BaseURL != "" {
		return fmt.Errorf("base_url cannot be set with chatgpt_login: the OAuth token is only ever sent to ChatGPT")
	}
	return nil
}

// TypeInfo is the machine-readable slice of a provider definition
// served to config UIs: which backends exist, what auth they offer, and which
// request features fail loudly on them. Display copy (labels, placeholders)
// deliberately stays in the frontend — this is facts, not wording.
type TypeInfo struct {
	Type string `json:"type"`
	// AuthModes and Unsupported serialize as [] rather than being omitted:
	// a client caching these facts must be able to tell "this backend has
	// none" apart from "not fetched yet" — omitempty would make an emptied
	// list look like missing data and let a stale local fallback win.
	AuthModes   []string `json:"auth_modes"`
	Unsupported []string `json:"unsupported"`
}

// Types lists the registered backends. The order is the registry's
// (openai first), which UIs may rely on for a default. Slices are copies —
// a caller mutating its result must not reach the registry.
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
