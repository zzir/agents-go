package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
)

func TestIsOpenAIModelName(t *testing.T) {
	cases := map[string]bool{
		"gpt-4.1": true, "gpt-4o": true, "o3": true, "o1-mini": true,
		"ft:gpt-4.1:org": true, "claude-opus": false, "": false, "llama3": false,
	}
	for model, want := range cases {
		if got := isOpenAIModelName(model); got != want {
			t.Errorf("isOpenAIModelName(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestResolveCompactionMode(t *testing.T) {
	f := false
	tr := true
	if m := resolveCompactionMode(CompactionModeAuto, "resp_1", &tr); m != CompactionModePreviousResponseID {
		t.Errorf("auto+stored = %v", m)
	}
	if m := resolveCompactionMode(CompactionModeAuto, "resp_1", &f); m != CompactionModeInput {
		t.Errorf("auto+unstored = %v", m)
	}
	if m := resolveCompactionMode(CompactionModeAuto, "", nil); m != CompactionModeInput {
		t.Errorf("auto+no-id = %v", m)
	}
	if m := resolveCompactionMode(CompactionModeInput, "resp_1", &tr); m != CompactionModeInput {
		t.Errorf("explicit mode should be preserved, got %v", m)
	}
}

func TestCompactionCandidateCount(t *testing.T) {
	items := []agents.TResponseInputItem{}
	items = append(items, agents.InputItemsFromText("user question")...) // user → excluded
	items = append(items, mustInput(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}`))
	items = append(items, mustInput(t, `{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`))
	items = append(items, mustInput(t, `{"type":"compaction"}`)) // compaction → excluded
	if got := compactionCandidateCount(items); got != 2 {
		t.Errorf("candidates = %d, want 2 (assistant + function_call)", got)
	}
}

func TestNewCompactionSessionRejects(t *testing.T) {
	under := agents.NewInMemorySession()
	if _, err := NewCompactionSession(under, CompactionOptions{Model: "claude-3"}); err == nil {
		t.Error("non-OpenAI model should be rejected")
	}
	conv := NewConversationsSession(option.WithAPIKey("x"))
	if _, err := NewCompactionSession(conv, CompactionOptions{}); err == nil {
		t.Error("wrapping a ConversationsSession should be rejected")
	}
}

// TestRunCompactionReplacesHistory drives RunCompaction against a stub
// responses.compact endpoint and verifies the underlying history is replaced.
func TestRunCompactionReplacesHistory(t *testing.T) {
	ctx := t.Context()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		// Return a single compacted assistant message as the output.
		resp := map[string]any{
			"id":         "resp_compacted",
			"created_at": 0,
			"object":     "response.compaction",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant", "id": "msg_c", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "summary", "annotations": []any{}}},
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	under := agents.NewInMemorySession()
	// Seed enough candidate items to clear the threshold.
	seed := []agents.TResponseInputItem{}
	for range 12 {
		seed = append(seed, mustInput(t, `{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`))
	}
	if err := under.AddItems(ctx, seed); err != nil {
		t.Fatal(err)
	}

	sess, err := NewCompactionSession(under, CompactionOptions{Model: "gpt-4.1", Mode: CompactionModeInput},
		option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}

	if err := sess.RunCompaction(ctx, agents.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}

	items, err := under.GetItems(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("after compaction len = %d, want 1 (the summary)", len(items))
	}
	b, _ := agents.MarshalInputItem(items[0])
	if !contains(string(b), "summary") {
		t.Errorf("compacted item = %s", b)
	}
}

// TestRunCompactionBelowThreshold verifies the API is not called when there are
// too few candidate items.
func TestRunCompactionBelowThreshold(t *testing.T) {
	ctx := t.Context()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	under := agents.NewInMemorySession()
	_ = under.AddItems(ctx, []agents.TResponseInputItem{
		mustInput(t, `{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`),
	})
	sess, err := NewCompactionSession(under, CompactionOptions{Model: "gpt-4.1"},
		option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RunCompaction(ctx, agents.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("responses.compact should not be called below the threshold")
	}
}

func mustInput(t *testing.T, raw string) agents.TResponseInputItem {
	t.Helper()
	item, err := agents.UnmarshalInputItem([]byte(raw))
	if err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return item
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
