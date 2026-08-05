package session

// RequestUsage is one model request's token accounting, as carried on entries
// and reported per response id. It is the flat, serializable form; the run's
// live accumulator (agents.Usage) produces one via its Request method.

// InputTokensDetails mirrors the OpenAI Responses API usage breakdown for input
// tokens. Only the fields the runner cares about are modeled.
type InputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
	// CacheWriteTokens counts input tokens written to the prompt cache (surfaced
	// by providers that bill cache writes separately). Older serialized RunState
	// snapshots without the field decode to zero.
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// OutputTokensDetails mirrors the OpenAI Responses API usage breakdown for
// output tokens.
type OutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// RequestUsage holds the token usage for a single model request.
type RequestUsage struct {
	InputTokens         int64               `json:"input_tokens"`
	OutputTokens        int64               `json:"output_tokens"`
	TotalTokens         int64               `json:"total_tokens"`
	InputTokensDetails  InputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails OutputTokensDetails `json:"output_tokens_details"`
}
