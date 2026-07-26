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

// Snapshot returns a point-in-time copy of u's counters taken under the same
// lock Add uses, so it is safe to call while other goroutines accumulate into u
// concurrently — e.g. reading a shared parent run's usage while parallel
// agent-as-tool nested runs fold theirs in with Add.
//
// The exported counter fields may be read directly only when no goroutine can
// be calling Add at the same time (for example, after a run has fully
// completed); reading them while Add runs concurrently is a data race that the
// race detector flags. Use Snapshot for the concurrent case. The returned value
// is standalone: RequestUsageEntries is a fresh slice, so it needs no further
// synchronization and does not alias u's storage. (Its zero-value mutex is
// unused; copy the returned value only by field, not wholesale.)
func (u *Usage) Snapshot() Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	var entries []RequestUsage
	if len(u.RequestUsageEntries) > 0 {
		entries = make([]RequestUsage, len(u.RequestUsageEntries))
		copy(entries, u.RequestUsageEntries)
	}
	return Usage{
		Requests:            u.Requests,
		InputTokens:         u.InputTokens,
		InputTokensDetails:  u.InputTokensDetails,
		OutputTokens:        u.OutputTokens,
		OutputTokensDetails: u.OutputTokensDetails,
		TotalTokens:         u.TotalTokens,
		RequestUsageEntries: entries,
	}
}

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

// Request flattens the aggregate into a single RequestUsage.
//
// It is what a nested run's total looks like from the outside: the caller of an
// agent-as-tool does not care that it took four calls, only what the call cost.
func (u *Usage) Request() RequestUsage {
	if u == nil {
		return RequestUsage{}
	}
	s := u.Snapshot()
	return RequestUsage{
		InputTokens:         s.InputTokens,
		OutputTokens:        s.OutputTokens,
		TotalTokens:         s.TotalTokens,
		InputTokensDetails:  s.InputTokensDetails,
		OutputTokensDetails: s.OutputTokensDetails,
	}
}

// attributeUsage puts each response's usage on exactly ONE of the entries it
// produced, so summing entry usage counts every request once.
//
// The last entry of the batch gets it. That is what makes an estimate of "how
// large is this conversation now" exact: a reader walks back to the most recent
// entry carrying usage, takes its input+output tokens as measured fact, and
// estimates only what follows. Usage on the FIRST entry of a response would
// make the rest of that response get estimated on top of a number that already
// counts it.
//
// A turn split across two batches — what an approval pause creates — attributes
// on the first batch and clears the flag, since a request counted twice is
// worse than one attributed a few entries early.
func (r *runner) attributeUsage(entries []SessionEntry) {
	if !r.usagePending || len(entries) == 0 {
		return
	}
	u := r.lastRequestUsage()
	if u == nil {
		r.usagePending = false
		return
	}
	for i := len(entries) - 1; i >= 0; i-- {
		// Match the response when the provider named one; a backend that
		// returns no id still gets its usage recorded, on the batch's last
		// entry, rather than silently losing it.
		if r.lastResponseID != "" && entries[i].ResponseID != r.lastResponseID {
			continue
		}
		entries[i].Usage = u
		r.usagePending = false
		return
	}
}

// lastRequestUsage returns the usage of the run's most recent model call, or
// nil when it reported none.
func (r *runner) lastRequestUsage() *RequestUsage {
	if r.lastUsage == nil {
		return nil
	}
	u := r.lastUsage.Request()
	if u.TotalTokens == 0 && u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	return &u
}

// usageOr returns u or an empty aggregate, so a log site can read a field off a
// possibly-nil usage without a branch.
func usageOr(u *Usage) RequestUsage {
	if u == nil {
		return RequestUsage{}
	}
	return u.Request()
}
