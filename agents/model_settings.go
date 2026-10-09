package agents

// ToolChoice constrains which tool the model must call: "auto", "required",
// "none" or a specific tool name (an open set); empty leaves it unset.
type ToolChoice string

// The predefined tool-choice modes.
const (
	ToolChoiceAuto     ToolChoice = "auto"
	ToolChoiceRequired ToolChoice = "required"
	ToolChoiceNone     ToolChoice = "none"
)

// Truncation is a truncation strategy for the Responses API: "auto" or
// "disabled". Empty means unset.
type Truncation string

// Truncation strategies for the Responses API.
const (
	TruncationAuto     Truncation = "auto"
	TruncationDisabled Truncation = "disabled"
)

// Verbosity constrains response verbosity: "low", "medium" or "high". Empty
// means unset.
type Verbosity string

// The predefined verbosity levels.
const (
	VerbosityLow    Verbosity = "low"
	VerbosityMedium Verbosity = "medium"
	VerbosityHigh   Verbosity = "high"
)

// ServiceTier selects the processing tier: "auto", "default", "flex" or
// "priority". Empty means unset.
type ServiceTier string

// The predefined service tiers.
const (
	ServiceTierAuto     ServiceTier = "auto"
	ServiceTierDefault  ServiceTier = "default"
	ServiceTierFlex     ServiceTier = "flex"
	ServiceTierPriority ServiceTier = "priority"
)

// PromptCacheRetention controls prompt-cache retention: "in_memory" or "24h".
// Empty means unset.
type PromptCacheRetention string

// The predefined prompt-cache retention policies.
const (
	PromptCacheRetentionInMemory PromptCacheRetention = "in_memory"
	PromptCacheRetention24h      PromptCacheRetention = "24h"
)

// PromptCacheMode controls whether the provider creates an implicit cache
// breakpoint: "implicit" (default) or "explicit". Empty means unset.
type PromptCacheMode string

// The predefined prompt-cache modes.
const (
	PromptCacheModeImplicit PromptCacheMode = "implicit"
	PromptCacheModeExplicit PromptCacheMode = "explicit"
)

// PromptCacheOptions configures prompt caching for OpenAI Responses API requests.
type PromptCacheOptions struct {
	// Mode is "implicit" (default) or "explicit". Empty leaves it unset.
	Mode PromptCacheMode `json:"mode,omitempty"`
	// TTL is the minimum cache-entry lifetime, e.g. "30m". Empty leaves it unset.
	TTL string `json:"ttl,omitempty"`
}

// ReasoningEffort constrains reasoning effort; an adapter rejects a value its
// backend cannot express.
type ReasoningEffort string

// Reasoning effort levels.
const (
	ReasoningEffortNone    ReasoningEffort = "none"
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortLow     ReasoningEffort = "low"
	ReasoningEffortMedium  ReasoningEffort = "medium"
	ReasoningEffortHigh    ReasoningEffort = "high"
	ReasoningEffortXhigh   ReasoningEffort = "xhigh"
	ReasoningEffortMax     ReasoningEffort = "max"
)

// ReasoningSummary selects the reasoning summary style: "auto", "concise" or
// "detailed". Empty means unset.
type ReasoningSummary string

// The predefined reasoning-summary styles.
const (
	ReasoningSummaryAuto     ReasoningSummary = "auto"
	ReasoningSummaryConcise  ReasoningSummary = "concise"
	ReasoningSummaryDetailed ReasoningSummary = "detailed"
)

// ContextManagementType is a server-side context-management entry type.
// Currently only "compaction" is supported.
type ContextManagementType string

// The predefined context-management entry types.
const (
	ContextManagementCompaction ContextManagementType = "compaction"
)

// ContextManagement is one server-side context-management entry forwarded to
// the OpenAI Responses API.
type ContextManagement struct {
	// Type is the entry type. Currently only "compaction" is supported.
	Type ContextManagementType `json:"type"`
	// CompactThreshold is the token threshold at which compaction triggers. nil
	// leaves it unset.
	CompactThreshold *int64 `json:"compact_threshold,omitempty"`
}

// Reasoning configures reasoning models (the forwarded subset of the OpenAI
// Reasoning object).
type Reasoning struct {
	// Effort is "minimal", "low", "medium" or "high".
	Effort ReasoningEffort `json:"effort,omitempty"`
	// Summary is "auto", "concise" or "detailed".
	Summary ReasoningSummary `json:"summary,omitempty"`
}

// ModelSettings holds optional model configuration; a nil pointer or empty
// value leaves the field unset for the provider default.
type ModelSettings struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	// ToolChoice controls tool selection. Empty means unset.
	ToolChoice ToolChoice `json:"tool_choice,omitempty"`

	// ParallelToolCalls controls whether the model may emit several tool calls
	// in one turn.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`

	// Truncation is "auto" or "disabled". Empty means unset.
	Truncation Truncation `json:"truncation,omitempty"`

	// MaxTokens is the maximum number of output tokens to generate.
	MaxTokens *int64 `json:"max_tokens,omitempty"`

	// Reasoning configures reasoning models.
	Reasoning *Reasoning `json:"reasoning,omitempty"`

	// Verbosity constrains response verbosity: "low", "medium" or "high".
	Verbosity Verbosity `json:"verbosity,omitempty"`

	// Metadata is included with the model response call.
	Metadata map[string]string `json:"metadata,omitempty"`

	// ServiceTier selects the processing tier: "auto", "default", "flex" or "priority".
	ServiceTier ServiceTier `json:"service_tier,omitempty"`

	// Store controls whether the provider stores the response for later retrieval.
	Store *bool `json:"store,omitempty"`

	// PromptCacheRetention is "in_memory" or "24h".
	PromptCacheRetention PromptCacheRetention `json:"prompt_cache_retention,omitempty"`

	// PromptCacheKey is forwarded as the Responses API prompt_cache_key; the
	// runner never generates one.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`

	// PromptCacheOptions configures prompt caching for OpenAI Responses API requests.
	PromptCacheOptions *PromptCacheOptions `json:"prompt_cache_options,omitempty"`

	// ContextManagement configures server-side context management for OpenAI
	// Responses API requests.
	ContextManagement []ContextManagement `json:"context_management,omitempty"`

	// ResponseInclude lists additional output data to include in the response.
	ResponseInclude []string `json:"response_include,omitempty"`

	// TopLogprobs requests logprobs for the top N tokens.
	TopLogprobs *int64 `json:"top_logprobs,omitempty"`

	// ExtraHeaders, ExtraQuery and ExtraBody are forwarded verbatim to the
	// provider request.
	ExtraHeaders map[string]string `json:"-"`
	ExtraQuery   map[string]string `json:"-"`
	ExtraBody    map[string]any    `json:"-"`
}

// Resolve returns a new ModelSettings overlaying every set field of override
// on the receiver; neither is mutated.
func (m *ModelSettings) Resolve(override *ModelSettings) *ModelSettings {
	if m == nil {
		m = &ModelSettings{}
	}
	out := *m
	if override == nil {
		return &out
	}

	if override.Temperature != nil {
		out.Temperature = override.Temperature
	}
	if override.TopP != nil {
		out.TopP = override.TopP
	}
	if override.ToolChoice != "" {
		out.ToolChoice = override.ToolChoice
	}
	if override.ParallelToolCalls != nil {
		out.ParallelToolCalls = override.ParallelToolCalls
	}
	if override.Truncation != "" {
		out.Truncation = override.Truncation
	}
	if override.MaxTokens != nil {
		out.MaxTokens = override.MaxTokens
	}
	if override.Reasoning != nil {
		out.Reasoning = override.Reasoning
	}
	if override.Verbosity != "" {
		out.Verbosity = override.Verbosity
	}
	if override.Metadata != nil {
		out.Metadata = override.Metadata
	}
	if override.ServiceTier != "" {
		out.ServiceTier = override.ServiceTier
	}
	if override.Store != nil {
		out.Store = override.Store
	}
	if override.PromptCacheRetention != "" {
		out.PromptCacheRetention = override.PromptCacheRetention
	}
	if override.PromptCacheKey != "" {
		out.PromptCacheKey = override.PromptCacheKey
	}
	if override.PromptCacheOptions != nil {
		out.PromptCacheOptions = override.PromptCacheOptions
	}
	if override.ContextManagement != nil {
		out.ContextManagement = override.ContextManagement
	}
	if override.ResponseInclude != nil {
		out.ResponseInclude = override.ResponseInclude
	}
	if override.TopLogprobs != nil {
		out.TopLogprobs = override.TopLogprobs
	}
	// The Extra maps are replaced wholesale, not merged per key.
	if override.ExtraHeaders != nil {
		out.ExtraHeaders = override.ExtraHeaders
	}
	if override.ExtraQuery != nil {
		out.ExtraQuery = override.ExtraQuery
	}
	if override.ExtraBody != nil {
		out.ExtraBody = override.ExtraBody
	}
	return &out
}
