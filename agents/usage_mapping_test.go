package agents_test

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
)

// UsageFromResponseUsage is the ONE mapping the streaming runner, the blocking
// OpenAI path and the modelkit conformance suite all share, so every field it
// carries is pinned here. A detail field that stopped being copied used to
// break one of three hand-written copies; now it would break all three at
// once, and this is what catches it.
func TestUsageFromResponseUsage(t *testing.T) {
	const raw = `{
		"input_tokens": 100,
		"output_tokens": 40,
		"total_tokens": 140,
		"input_tokens_details": {"cached_tokens": 60, "cache_write_tokens": 20},
		"output_tokens_details": {"reasoning_tokens": 16}
	}`
	var ru responses.ResponseUsage
	if err := json.Unmarshal([]byte(raw), &ru); err != nil {
		t.Fatal(err)
	}

	u := agents.UsageFromResponseUsage(ru)
	for _, f := range []struct {
		name      string
		got, want int64
	}{
		{"Requests", u.Requests, 1},
		{"InputTokens", u.InputTokens, 100},
		{"OutputTokens", u.OutputTokens, 40},
		{"TotalTokens", u.TotalTokens, 140},
		{"InputTokensDetails.CachedTokens", u.InputTokensDetails.CachedTokens, 60},
		{"InputTokensDetails.CacheWriteTokens", u.InputTokensDetails.CacheWriteTokens, 20},
		{"OutputTokensDetails.ReasoningTokens", u.OutputTokensDetails.ReasoningTokens, 16},
	} {
		if f.got != f.want {
			t.Errorf("%s = %d, want %d", f.name, f.got, f.want)
		}
	}
}

// A usage block that reports all zeros is still ONE request. Deciding that a
// response carries no usage block at all — which is zero requests — is the
// caller's job, not the mapping's; the runner and the OpenAI adapter each
// guard for it before calling.
func TestUsageFromResponseUsage_ZeroBlockIsStillOneRequest(t *testing.T) {
	u := agents.UsageFromResponseUsage(responses.ResponseUsage{})
	if u.Requests != 1 || u.TotalTokens != 0 {
		t.Errorf("Requests=%d Total=%d, want 1/0", u.Requests, u.TotalTokens)
	}
}
