package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/tracing"
)

// --- ConversationsSession.AddItems batching -------------------------------

// itemBatchRecorder fakes the Conversations endpoints AddItems touches and
// records the item count of every POST /conversations/{id}/items request.
type itemBatchRecorder struct {
	mu      sync.Mutex
	batches []int
	stored  []map[string]any
	seq     int
}

func (f *itemBatchRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /conversations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "conv_batch", "object": "conversation"})
	})
	mux.HandleFunc("POST /conversations/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.batches = append(f.batches, len(body.Items))
		created := []map[string]any{}
		for _, it := range body.Items {
			f.seq++
			it["id"] = "item_" + strconv.Itoa(f.seq)
			f.stored = append(f.stored, it)
			created = append(created, it)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": created})
	})
	return mux
}

func TestConversationsSession_AddItemsBatchesAtAPILimit(t *testing.T) {
	ctx := t.Context()
	fake := &itemBatchRecorder{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	s := NewConversationsSession(option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))

	items := make([]agents.TResponseInputItem, 0, 45)
	for i := range 45 {
		items = append(items, agents.InputItemsFromText("msg-"+strconv.Itoa(i))...)
	}
	if err := agents.NewSession(s).AppendItems(ctx, items, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	batches := append([]int(nil), fake.batches...)
	stored := len(fake.stored)
	fake.mu.Unlock()

	want := []int{20, 20, 5}
	if len(batches) != len(want) {
		t.Fatalf("batches = %v, want %v", batches, want)
	}
	for i, n := range want {
		if batches[i] != n {
			t.Fatalf("batches = %v, want %v", batches, want)
		}
	}
	if stored != 45 {
		t.Errorf("stored items = %d, want 45", stored)
	}
}

func TestConversationsSession_AddItemsSingleBatchUnderLimit(t *testing.T) {
	ctx := t.Context()
	fake := &itemBatchRecorder{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	s := NewConversationsSession(option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))

	if err := agents.NewSession(s).AppendItems(ctx, agents.InputItemsFromText("only"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.batches) != 1 || fake.batches[0] != 1 {
		t.Errorf("batches = %v, want [1]", fake.batches)
	}
}

// --- CompactionSession tracing span ----------------------------------------

// newCompactStub serves a minimal responses.compact endpoint whose output is a
// single compacted assistant message.
func newCompactStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
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
	return srv
}

func seededCompactionSession(t *testing.T, baseURL string, n int, under agents.SessionStorage) *CompactionSession {
	t.Helper()
	ctx := t.Context()
	seed := make([]agents.TResponseInputItem, 0, n)
	for range n {
		seed = append(seed, mustInput(t, `{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`))
	}
	if err := agents.NewSession(under).AppendItems(ctx, seed, agents.Source{}); err != nil {
		t.Fatal(err)
	}
	sess, err := NewCompactionSession(under, CompactionOptions{Model: "gpt-4.1", Mode: CompactionModeInput},
		option.WithAPIKey("test"), option.WithBaseURL(baseURL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestRunCompactionStartsSpanWithCounts(t *testing.T) {
	ctx := t.Context()
	srv := newCompactStub(t)
	sess := seededCompactionSession(t, srv.URL, 12, agents.NewInMemorySession())

	span := &tracing.SpanHandle{Span: &tracing.Span{Data: map[string]any{}}}
	startCalls := 0
	args := agents.CompactionArgs{
		ResponseID: "resp_1",
		StartSpan: func() *tracing.SpanHandle {
			startCalls++
			return span
		},
	}
	if err := sess.RunCompaction(ctx, args); err != nil {
		t.Fatal(err)
	}
	if startCalls != 1 {
		t.Fatalf("StartSpan calls = %d, want 1", startCalls)
	}
	if got := span.Span.Data["before_items"]; got != 12 {
		t.Errorf("before_items = %v, want 12", got)
	}
	if got := span.Span.Data["after_items"]; got != 1 {
		t.Errorf("after_items = %v, want 1", got)
	}
}

func TestRunCompactionNoSpanOnNoOpPass(t *testing.T) {
	ctx := t.Context()
	srv := newCompactStub(t)
	// One candidate item: far below the default threshold, so no compaction.
	sess := seededCompactionSession(t, srv.URL, 1, agents.NewInMemorySession())

	startCalls := 0
	args := agents.CompactionArgs{
		ResponseID: "resp_1",
		StartSpan: func() *tracing.SpanHandle {
			startCalls++
			return &tracing.SpanHandle{}
		},
	}
	if err := sess.RunCompaction(ctx, args); err != nil {
		t.Fatal(err)
	}
	if startCalls != 0 {
		t.Errorf("StartSpan calls = %d, want 0 on the no-op path", startCalls)
	}
}

func TestRunCompactionNilStartSpanIsSafe(t *testing.T) {
	ctx := t.Context()
	srv := newCompactStub(t)
	under := agents.NewInMemoryStorage("test")
	sess := seededCompactionSession(t, srv.URL, 12, under)

	if err := sess.RunCompaction(ctx, agents.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}
	items, err := agents.NewSession(under).ContextItems(ctx, agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("after compaction len = %d, want 1", len(items))
	}
}

// --- CompactionSession atomic history replacement --------------------------

// replaceRecordingSession wraps InMemorySession, counting Clear and
// ReplaceItems calls to prove which write path compaction uses.
type replaceRecordingSession struct {
	*agents.InMemoryStorage
	mu           sync.Mutex
	clearCalls   int
	replaceCalls int
}

func (r *replaceRecordingSession) Clear(ctx context.Context) error {
	r.mu.Lock()
	r.clearCalls++
	r.mu.Unlock()
	return r.InMemoryStorage.Clear(ctx)
}

func (r *replaceRecordingSession) ReplaceEntries(ctx context.Context, entries ...agents.SessionEntry) error {
	r.mu.Lock()
	r.replaceCalls++
	r.mu.Unlock()
	return r.InMemoryStorage.ReplaceEntries(ctx, entries...)
}

func TestRunCompactionUsesAtomicReplace(t *testing.T) {
	ctx := t.Context()
	srv := newCompactStub(t)
	under := &replaceRecordingSession{InMemoryStorage: agents.NewInMemoryStorage("test")}
	sess := seededCompactionSession(t, srv.URL, 12, under)

	if err := sess.RunCompaction(ctx, agents.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}

	under.mu.Lock()
	clearCalls, replaceCalls := under.clearCalls, under.replaceCalls
	under.mu.Unlock()
	if replaceCalls != 1 {
		t.Errorf("ReplaceItems calls = %d, want 1", replaceCalls)
	}
	if clearCalls != 0 {
		t.Errorf("Clear calls = %d, want 0 (history must not go through a clear+add window)", clearCalls)
	}

	items, err := agents.NewSession(under).ContextItems(ctx, agents.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("after compaction len = %d, want 1", len(items))
	}
	b, _ := agents.MarshalInputItem(items[0])
	if !contains(string(b), "summary") {
		t.Errorf("compacted item = %s", b)
	}
}

// --- RetryAfter header parsing ----------------------------------------------

func TestRetryAfter_MsHeader(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{"Retry-After-Ms": []string{"1500"}})
	d, ok := RetryAfter(err)
	if !ok || d != 1500*time.Millisecond {
		t.Fatalf("d=%v ok=%v, want 1.5s true", d, ok)
	}
}

func TestRetryAfter_MsPreferredOverSeconds(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{
		"Retry-After-Ms": []string{"250"},
		"Retry-After":    []string{"7"},
	})
	d, ok := RetryAfter(err)
	if !ok || d != 250*time.Millisecond {
		t.Fatalf("d=%v ok=%v, want 250ms true (Retry-After-Ms wins)", d, ok)
	}
}

func TestRetryAfter_FloatSeconds(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"0.5"}})
	d, ok := RetryAfter(err)
	if !ok || d != 500*time.Millisecond {
		t.Fatalf("d=%v ok=%v, want 500ms true", d, ok)
	}
}

func TestRetryAfter_BadMsFallsThroughToSeconds(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{
		"Retry-After-Ms": []string{"soon"},
		"Retry-After":    []string{"2"},
	})
	d, ok := RetryAfter(err)
	if !ok || d != 2*time.Second {
		t.Fatalf("d=%v ok=%v, want 2s true (unparseable ms header skipped)", d, ok)
	}
}
