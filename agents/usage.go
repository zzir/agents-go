package agents

import (
	"sync"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents/session"
)

// Usage aggregates token usage across all model requests in a run.
//
// RequestUsageEntries preserves a per-request breakdown so callers can compute
// accurate costs even though the top-level counters are summed.
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

// UsageFromResponseUsage translates a Responses usage block into a Usage
// counted as ONE request.
//
// This is the single place that field-by-field translation lives. The
// streaming runner, the blocking OpenAI path and the modelkit conformance
// suite all go through it, so a detail field the Responses API adds later is
// picked up by all three at once instead of drifting in whichever hand-copy
// was missed.
//
// Whether the response carries a usage block AT ALL stays the caller's
// question. Not because the three ask it differently — they all ask
// resp.JSON.Usage.Valid() and they all answer NewUsage, zero requests, so a
// backend that never reports usage does not inflate the request count — but
// because this mapping is total: an all-zero usage block is a real request
// that spent no tokens, and only the caller, holding the Response envelope,
// can tell that apart from a response that reported nothing.
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

// Add aggregates another Usage into the receiver.
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
