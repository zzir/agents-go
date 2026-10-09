package session

// InputTokensDetails mirrors the Responses API usage breakdown for input
// tokens; only the fields the runner reads are modeled.
type InputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
	// CacheWriteTokens counts input tokens written to the prompt cache.
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// OutputTokensDetails mirrors the Responses API usage breakdown for output tokens.
type OutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// RequestUsage is one model request's token accounting, as carried on entries;
// agents.Usage.Request produces one from the run's live accumulator.
type RequestUsage struct {
	InputTokens         int64               `json:"input_tokens"`
	OutputTokens        int64               `json:"output_tokens"`
	TotalTokens         int64               `json:"total_tokens"`
	InputTokensDetails  InputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails OutputTokensDetails `json:"output_tokens_details"`
}
