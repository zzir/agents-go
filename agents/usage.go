package agents

import "sync"

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

// Usage aggregates token usage across all model requests in a run.
//
// It is the Go counterpart of the Python SDK's Usage dataclass. RequestUsageEntries
// preserves a per-request breakdown so callers can compute accurate costs even
// though the top-level counters are summed.
type Usage struct {
	// Requests is the total number of LLM API calls made.
	Requests int64 `json:"requests"`
	// InputTokens is the total input tokens sent across all requests.
	InputTokens int64 `json:"input_tokens"`
	// InputTokensDetails aggregates the input token detail fields.
	InputTokensDetails InputTokensDetails `json:"input_tokens_details"`
	// OutputTokens is the total output tokens received across all requests.
	OutputTokens int64 `json:"output_tokens"`
	// OutputTokensDetails aggregates the output token detail fields.
	OutputTokensDetails OutputTokensDetails `json:"output_tokens_details"`
	// TotalTokens is the total tokens sent and received across all requests.
	TotalTokens int64 `json:"total_tokens"`
	// RequestUsageEntries preserves the per-request usage breakdown.
	RequestUsageEntries []RequestUsage `json:"request_usage_entries,omitempty"`

	// mu guards Add so concurrent accumulation is safe — e.g. several
	// agent-as-tool nested runs completing in parallel and folding their usage
	// into the shared parent Usage. The zero value is an unlocked mutex, so a
	// Usage literal (or NewUsage) needs no initialization.
	mu sync.Mutex
}

// NewUsage returns a zero-valued Usage ready to accumulate.
func NewUsage() *Usage { return &Usage{} }

// Add aggregates another Usage into the receiver, mirroring Python's Usage.add.
//
// Per-request entries are preserved: if other already carries entries they are
// appended; otherwise, if other represents a single request with tokens, a
// synthetic entry is recorded.
func (u *Usage) Add(other *Usage) {
	if other == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.Requests += other.Requests
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens
	u.InputTokensDetails.CachedTokens += other.InputTokensDetails.CachedTokens
	u.InputTokensDetails.CacheWriteTokens += other.InputTokensDetails.CacheWriteTokens
	u.OutputTokensDetails.ReasoningTokens += other.OutputTokensDetails.ReasoningTokens

	switch {
	case len(other.RequestUsageEntries) > 0:
		u.RequestUsageEntries = append(u.RequestUsageEntries, other.RequestUsageEntries...)
	case other.Requests == 1 && other.TotalTokens > 0:
		u.RequestUsageEntries = append(u.RequestUsageEntries, RequestUsage{
			InputTokens:         other.InputTokens,
			OutputTokens:        other.OutputTokens,
			TotalTokens:         other.TotalTokens,
			InputTokensDetails:  other.InputTokensDetails,
			OutputTokensDetails: other.OutputTokensDetails,
		})
	}
}
