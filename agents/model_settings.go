package agents

// ToolChoice constrains which tool (if any) the model must call.
//
// Use the predefined constants ("auto", "required", "none") or a specific tool
// name. The zero value (empty string) means "leave unset" and defers to the
// provider default.
type ToolChoice = string

// The predefined tool-choice modes.
const (
	ToolChoiceAuto     ToolChoice = "auto"
	ToolChoiceRequired ToolChoice = "required"
	ToolChoiceNone     ToolChoice = "none"
)

// Truncation strategies for the Responses API.
const (
	TruncationAuto     = "auto"
	TruncationDisabled = "disabled"
)

// Reasoning configures reasoning models. It mirrors the subset of the OpenAI
// shared Reasoning object that the runner forwards to the provider.
type Reasoning struct {
	// Effort is "minimal", "low", "medium" or "high".
	Effort string `json:"effort,omitempty"`
	// Summary is "auto", "concise" or "detailed".
	Summary string `json:"summary,omitempty"`
}

// ModelSettings holds optional model configuration parameters (temperature,
// top_p, penalties, truncation, etc). Not every model or provider supports every
// field. All fields are optional; a nil pointer means "leave unset" so the
// provider default applies.
//
// It is the Go counterpart of the Python SDK's ModelSettings dataclass.
type ModelSettings struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`

	// ToolChoice controls tool selection. Empty means unset.
	ToolChoice ToolChoice `json:"tool_choice,omitempty"`

	// ParallelToolCalls controls whether the model may emit multiple tool calls
	// in a single turn. nil defers to the provider default.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`

	// Truncation is "auto" or "disabled". Empty means unset.
	Truncation string `json:"truncation,omitempty"`

	// MaxTokens is the maximum number of output tokens to generate.
	MaxTokens *int64 `json:"max_tokens,omitempty"`

	// Reasoning configures reasoning models.
	Reasoning *Reasoning `json:"reasoning,omitempty"`

	// Verbosity constrains response verbosity: "low", "medium" or "high".
	Verbosity string `json:"verbosity,omitempty"`

	// Metadata is included with the model response call.
	Metadata map[string]string `json:"metadata,omitempty"`

	// ServiceTier selects the processing tier: "auto", "default", "flex" or "priority".
	ServiceTier string `json:"service_tier,omitempty"`

	// Store controls whether the provider stores the response for later retrieval.
	Store *bool `json:"store,omitempty"`

	// PromptCacheRetention is "in_memory" or "24h".
	PromptCacheRetention string `json:"prompt_cache_retention,omitempty"`

	// ResponseInclude lists additional output data to include in the response.
	ResponseInclude []string `json:"response_include,omitempty"`

	// TopLogprobs requests logprobs for the top N tokens.
	TopLogprobs *int64 `json:"top_logprobs,omitempty"`

	// ExtraHeaders, ExtraQuery and ExtraBody are forwarded verbatim to the
	// underlying provider request.
	ExtraHeaders map[string]string `json:"-"`
	ExtraQuery   map[string]string `json:"-"`
	ExtraBody    map[string]any    `json:"-"`
}

// Resolve returns a new ModelSettings produced by overlaying every set
// (non-nil / non-empty) field of override on top of the receiver. It mirrors
// Python's ModelSettings.resolve. The receiver and override are not mutated.
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
	if override.FrequencyPenalty != nil {
		out.FrequencyPenalty = override.FrequencyPenalty
	}
	if override.PresencePenalty != nil {
		out.PresencePenalty = override.PresencePenalty
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
	if override.ResponseInclude != nil {
		out.ResponseInclude = override.ResponseInclude
	}
	if override.TopLogprobs != nil {
		out.TopLogprobs = override.TopLogprobs
	}
	if override.ExtraHeaders != nil {
		out.ExtraHeaders = mergeStringMap(out.ExtraHeaders, override.ExtraHeaders)
	}
	if override.ExtraQuery != nil {
		out.ExtraQuery = mergeStringMap(out.ExtraQuery, override.ExtraQuery)
	}
	if override.ExtraBody != nil {
		out.ExtraBody = mergeAnyMap(out.ExtraBody, override.ExtraBody)
	}
	return &out
}

func mergeStringMap(base, over map[string]string) map[string]string {
	if base == nil && over == nil {
		return nil
	}
	merged := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range over {
		merged[k] = v
	}
	return merged
}

func mergeAnyMap(base, over map[string]any) map[string]any {
	if base == nil && over == nil {
		return nil
	}
	merged := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range over {
		merged[k] = v
	}
	return merged
}

// Ptr is a small helper that returns a pointer to v. It is convenient for
// setting optional ModelSettings fields, e.g. Temperature: agents.Ptr(0.7).
func Ptr[T any](v T) *T { return &v }
