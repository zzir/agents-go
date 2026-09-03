package agents

import (
	"sync"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

// Usage aggregates token usage across all model requests in a run;
// RequestUsageEntries keeps the per-request breakdown for accurate costs.
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

	// mu guards Add: parallel agent-as-tool runs fold usage into a shared
	// parent. The zero value is unlocked, so a literal needs no init.
	mu sync.Mutex
}

// NewUsage returns a zero-valued Usage ready to accumulate.
func NewUsage() *Usage { return &Usage{} }

// UsageFromResponseUsage translates a Responses usage block into a Usage
// counted as ONE request; it is the single place the field mapping lives.
// Whether the response carried a usage block at all is the caller's question:
// an all-zero block is a real request that spent no tokens.
func UsageFromResponseUsage(u responses.ResponseUsage) *Usage {
	return &Usage{
		Requests:     1,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
		InputTokensDetails: InputTokensDetails{
			CachedTokens:     u.InputTokensDetails.CachedTokens,
			CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
		},
		OutputTokensDetails: OutputTokensDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens},
	}
}

// Snapshot returns a point-in-time copy of u's counters under the same lock
// Add uses, so it is safe while other goroutines accumulate into u. Read the
// exported fields directly only when no goroutine can be calling Add. The copy
// is standalone (RequestUsageEntries is a fresh slice); copy it by field, not
// wholesale, as its zero-value mutex is unused.
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

// Add aggregates another Usage into the receiver. Per-request entries are
// preserved: other's entries are appended, or a synthetic entry is recorded
// when other is a single request with tokens.
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

// Request flattens the aggregate into a single RequestUsage — what a nested
// run's total looks like from the outside.
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

// attributeUsage puts each response's usage on exactly one entry — the last of
// the batch, once even across a split turn — see spec §2.7f.
func (r *runner) attributeUsage(entries []session.Entry) {
	if !r.usagePending || len(entries) == 0 {
		return
	}
	u := r.lastRequestUsage()
	if u == nil {
		r.usagePending = false
		return
	}
	for i := len(entries) - 1; i >= 0; i-- {
		// A backend returning no id still records usage on the batch's last
		// entry; caller input in the batch (an injection) is not the response's.
		if entries[i].Source.Type == SourceUser ||
			r.lastResponseID != "" && entries[i].ResponseID != r.lastResponseID {
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
