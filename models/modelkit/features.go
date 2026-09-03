package modelkit

import (
	"github.com/zzir/agents-go/agents"
)

// Feature identifies one optional capability of a ModelRequest — a
// ModelSettings field or a request-level field — that not every backend has an
// equivalent for. The names are the wire/JSON names users know from
// configuration.
type Feature string

// The request features an adapter can declare unsupported.
const (
	FeatureTemperature          Feature = "temperature"
	FeatureTopP                 Feature = "top_p"
	FeatureToolChoice           Feature = "tool_choice"
	FeatureParallelToolCalls    Feature = "parallel_tool_calls"
	FeatureTruncation           Feature = "truncation"
	FeatureMaxTokens            Feature = "max_tokens"
	FeatureReasoning            Feature = "reasoning"
	FeatureReasoningSummary     Feature = "reasoning.summary"
	FeatureVerbosity            Feature = "verbosity"
	FeatureMetadata             Feature = "metadata"
	FeatureServiceTier          Feature = "service_tier"
	FeatureStore                Feature = "store"
	FeaturePromptCacheRetention Feature = "prompt_cache_retention"
	FeaturePromptCacheKey       Feature = "prompt_cache_key"
	FeaturePromptCacheOptions   Feature = "prompt_cache_options"
	FeatureContextManagement    Feature = "context_management"
	FeatureResponseInclude      Feature = "response_include"
	FeatureTopLogprobs          Feature = "top_logprobs"
	FeaturePreviousResponseID   Feature = "previous_response_id"
	FeatureConversationID       Feature = "conversation_id"
	FeaturePrompt               Feature = "prompt"
	FeatureOutputSchema         Feature = "output_schema"
)

// featureSet reports whether a request actually exercises a feature: only set
// fields count, so an unsupported feature never rejects a request that skipped it.
var featureSet = map[Feature]func(agents.ModelRequest) bool{
	FeatureTemperature:       func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.Temperature != nil },
	FeatureTopP:              func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.TopP != nil },
	FeatureToolChoice:        func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.ToolChoice != "" },
	FeatureParallelToolCalls: func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.ParallelToolCalls != nil },
	FeatureTruncation:        func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.Truncation != "" },
	FeatureMaxTokens:         func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.MaxTokens != nil },
	FeatureReasoning:         func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.Reasoning != nil },
	FeatureReasoningSummary: func(r agents.ModelRequest) bool {
		return r.Settings != nil && r.Settings.Reasoning != nil && r.Settings.Reasoning.Summary != ""
	},
	FeatureVerbosity:            func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.Verbosity != "" },
	FeatureMetadata:             func(r agents.ModelRequest) bool { return r.Settings != nil && len(r.Settings.Metadata) > 0 },
	FeatureServiceTier:          func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.ServiceTier != "" },
	FeatureStore:                func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.Store != nil },
	FeaturePromptCacheRetention: func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.PromptCacheRetention != "" },
	FeaturePromptCacheKey:       func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.PromptCacheKey != "" },
	FeaturePromptCacheOptions:   func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.PromptCacheOptions != nil },
	FeatureContextManagement:    func(r agents.ModelRequest) bool { return r.Settings != nil && len(r.Settings.ContextManagement) > 0 },
	FeatureResponseInclude:      func(r agents.ModelRequest) bool { return r.Settings != nil && len(r.Settings.ResponseInclude) > 0 },
	FeatureTopLogprobs:          func(r agents.ModelRequest) bool { return r.Settings != nil && r.Settings.TopLogprobs != nil },
	FeaturePreviousResponseID:   func(r agents.ModelRequest) bool { return r.PreviousResponseID != "" },
	FeatureConversationID:       func(r agents.ModelRequest) bool { return r.ConversationID != "" },
	FeaturePrompt:               func(r agents.ModelRequest) bool { return r.Prompt != nil },
	FeatureOutputSchema:         func(r agents.ModelRequest) bool { return r.OutputSchema != nil && !r.OutputSchema.IsPlainText() },
}

// Reject returns a *agents.UserError naming the first of the given unsupported
// features the request actually uses, or nil when it uses none.
//
// This is the fail-loud half of the adapter contract: a setting the backend
// has no equivalent for must fail the call, not be dropped. A dropped setting
// is invisible — the user configured a behavior, nothing enforces it, and the
// first sign is production output that quietly ignores their config.
func Reject(provider string, req agents.ModelRequest, unsupported ...Feature) error {
	for _, f := range unsupported {
		isSet, ok := featureSet[f]
		if !ok {
			return agents.NewUserError("%s: unknown feature %q declared unsupported", provider, string(f))
		}
		if isSet(req) {
			return agents.NewUserError("%s: %s is not supported by this backend — unset it for this provider", provider, string(f))
		}
	}
	return nil
}

// Capabilities is a provider's static declaration of the request features it
// cannot serve. It exists so a hosting layer (e.g. a config UI) can surface
// limits before a run fails; the enforced truth remains Reject at call time.
type Capabilities struct {
	// Unsupported lists the features Reject is called with.
	Unsupported []Feature
}
