package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
	"github.com/zzir/agents-go/tracing"
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
	stored := session.CompactionArgs{ResponseID: "resp_1", Store: &tr}
	if m, ok := resolveCompactionMode(CompactionModeAuto, stored); m != CompactionModePreviousResponseID || !ok {
		t.Errorf("auto+stored = %v, %v", m, ok)
	}
	if m, ok := resolveCompactionMode(CompactionModeAuto, session.CompactionArgs{ResponseID: "resp_1", Store: &f}); m != CompactionModeInput || !ok {
		t.Errorf("auto+unstored = %v, %v", m, ok)
	}
	if m, ok := resolveCompactionMode(CompactionModeAuto, session.CompactionArgs{}); m != CompactionModeInput || !ok {
		t.Errorf("auto+no-id = %v, %v", m, ok)
	}
	if m, ok := resolveCompactionMode(CompactionModeInput, stored); m != CompactionModeInput || !ok {
		t.Errorf("explicit mode should be preserved, got %v, %v", m, ok)
	}

	// Items the chain cannot hold rule previous_response_id out: under auto the
	// compact call takes the stored items instead, and a caller who pinned the
	// chain mode has their pass skipped rather than silently redirected.
	offChain := session.CompactionArgs{ResponseID: "resp_1", Store: &tr, OffChainItems: true}
	if m, ok := resolveCompactionMode(CompactionModeAuto, offChain); m != CompactionModeInput || !ok {
		t.Errorf("auto+off-chain = %v, %v; want input, true", m, ok)
	}
	if _, ok := resolveCompactionMode(CompactionModePreviousResponseID, offChain); ok {
		t.Error("pinned previous_response_id + off-chain items should skip the pass")
	}
	if m, ok := resolveCompactionMode(CompactionModeInput, offChain); m != CompactionModeInput || !ok {
		t.Errorf("input mode is already safe for off-chain items, got %v, %v", m, ok)
	}
}

func TestCompactionCandidateCount(t *testing.T) {
	items := []agents.InputItem{}
	items = append(items, agents.InputItemsFromText("user question")...) // user → excluded
	items = append(items, mustInput(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}`))
	items = append(items, mustInput(t, `{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`))
	items = append(items, mustInput(t, `{"type":"compaction"}`)) // compaction → excluded
	if got := compactionCandidateCount(items); got != 2 {
		t.Errorf("candidates = %d, want 2 (assistant + function_call)", got)
	}
}

func TestNewCompactionSessionRejects(t *testing.T) {
	under := session.NewInMemoryStorage("test")
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

	under := session.NewInMemoryStorage("test")
	// Seed enough candidate items to clear the threshold.
	seed := []agents.InputItem{}
	for range 12 {
		seed = append(seed, mustInput(t, `{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`))
	}
	if err := session.NewSession(under).AppendItems(ctx, seed, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	sess, err := NewCompactionSession(under, CompactionOptions{Model: "gpt-4.1", Mode: CompactionModeInput},
		option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}

	if err := sess.RunCompaction(ctx, session.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}

	items, err := session.NewSession(under).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("after compaction len = %d, want 1 (the summary)", len(items))
	}
	b, _ := session.MarshalInputItem(items[0])
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

	under := session.NewInMemoryStorage("test")
	_ = session.NewSession(under).AppendItems(ctx, []agents.InputItem{
		mustInput(t, `{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`),
	}, agents.Source{})
	sess, err := NewCompactionSession(under, CompactionOptions{Model: "gpt-4.1"},
		option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RunCompaction(ctx, session.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("responses.compact should not be called below the threshold")
	}
}

// recordingCompactStub is newCompactStub that also keeps every request body, so
// a test can say HOW the conversation was handed to responses.compact.
func recordingCompactStub(t *testing.T, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*bodies = append(*bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_compacted", "created_at": 0, "object": "response.compaction",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant", "id": "msg_c", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "summary", "annotations": []any{}}},
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedOffChainLog builds a history whose last item is one the server's chain
// cannot hold — a tool output produced after the last model response, which is
// what CompactionArgs.OffChainItems reports.
func seedOffChainLog(t *testing.T, under session.Storage) {
	t.Helper()
	s := session.NewSession(under)
	if err := s.AppendItems(t.Context(), []agents.InputItem{
		mustInput(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling","annotations":[]}]}`),
		mustInput(t, `{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`),
	}, agents.Source{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendItems(t.Context(), []agents.InputItem{
		mustInput(t, `{"type":"function_call_output","call_id":"c1","output":"only-in-the-log"}`),
	}, agents.Source{Type: agents.SourceTool}); err != nil {
		t.Fatal(err)
	}
}

// Under "auto", a log holding items the chain never saw is compacted from the
// stored history instead. previous_response_id would replace those items with a
// summary written without them — deleted having never been read — while the
// input mode hands the compact call exactly what its answer stands in for.
func TestRunCompactionOffChainItemsCompactFromStoredHistory(t *testing.T) {
	ctx := t.Context()
	var bodies []map[string]any
	srv := recordingCompactStub(t, &bodies)

	under := session.NewInMemoryStorage("test")
	seedOffChainLog(t, under)
	sess, err := NewCompactionSession(under, CompactionOptions{Model: "gpt-4.1", Threshold: 1},
		option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}

	if err := sess.RunCompaction(ctx, session.CompactionArgs{ResponseID: "resp_1", OffChainItems: true}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("compact calls = %d, want 1", len(bodies))
	}
	if id, ok := bodies[0]["previous_response_id"]; ok {
		t.Errorf("compacted from the chain (%v) while the log held items it cannot show", id)
	}
	sent, _ := json.Marshal(bodies[0]["input"])
	if !contains(string(sent), "only-in-the-log") {
		t.Errorf("the off-chain item never reached the compact call: %s", sent)
	}
}

// A caller who pinned previous_response_id gets the pass SKIPPED rather than
// silently switched: the mode is the one thing they configured, and compaction
// is housekeeping that can always afford to wait.
func TestRunCompactionPinnedChainModeSkipsOffChainItems(t *testing.T) {
	ctx := t.Context()
	var bodies []map[string]any
	srv := recordingCompactStub(t, &bodies)

	under := session.NewInMemoryStorage("test")
	seedOffChainLog(t, under)
	sess, err := NewCompactionSession(under,
		CompactionOptions{Model: "gpt-4.1", Threshold: 1, Mode: CompactionModePreviousResponseID},
		option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}

	span := &tracing.SpanHandle{Span: &tracing.Span{Data: map[string]any{}}}
	err = sess.RunCompaction(ctx, session.CompactionArgs{
		ResponseID:    "resp_1",
		OffChainItems: true,
		StartSpan:     func() *tracing.SpanHandle { return span },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 0 {
		t.Errorf("compact calls = %d, want 0 — the pinned mode cannot see those items", len(bodies))
	}
	if got := span.Span.Data["abandoned"]; got != "off_chain_items" {
		t.Errorf("span abandoned = %v, want off_chain_items", got)
	}
	items, err := session.NewSession(under).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Errorf("history len = %d, want 3 — a skipped pass rewrites nothing", len(items))
	}
}

// A read window (session.Settings.Limit) is a permanent setting, not a passing
// condition, so what a windowed run reports about its chain decides whether a
// caller who PINNED previous_response_id ever compacts again. These two drive a
// whole run into a real CompactionSession, because that consequence is invisible
// at the runner boundary where the flag is produced.
func TestRunCompactionPinnedChainModeUnderAReadWindow(t *testing.T) {
	// windowedRun runs one turn against a pinned-mode CompactionSession holding
	// seeded entries, and returns the compact request bodies it made.
	windowedRun := func(t *testing.T, seed, limit int) []map[string]any {
		t.Helper()
		var bodies []map[string]any
		srv := recordingCompactStub(t, &bodies)
		under := session.NewInMemoryStorage("test")
		cs, err := NewCompactionSession(under,
			CompactionOptions{Model: "gpt-4.1", Threshold: 1, Mode: CompactionModePreviousResponseID},
			option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
		if err != nil {
			t.Fatal(err)
		}
		sess := session.NewSession(cs)
		var seeded []agents.InputItem
		for range seed {
			seeded = append(seeded, mustInput(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old","annotations":[]}]}`))
		}
		if err := sess.AppendItems(t.Context(), seeded, agents.Source{}); err != nil {
			t.Fatal(err)
		}
		agent := &agents.Agent{Name: "a", ModelImpl: agentstest.TextModel("answer")}
		if _, err := agents.RunSync(t.Context(), agent, "go", agents.RunOptions{
			Conversation: agents.ConversationOptions{
				Session: sess, Settings: session.Settings{Limit: limit},
			},
		}); err != nil {
			t.Fatal(err)
		}
		return bodies
	}

	// The window never truncated the read, so the chain saw the whole log and
	// the pinned mode is safe. Reporting a configured window as off-chain
	// regardless would abandon this pass — and every later one, since a window
	// never clears.
	t.Run("log inside the window", func(t *testing.T) {
		bodies := windowedRun(t, 2, 50)
		if len(bodies) != 1 {
			t.Fatalf("compact calls = %d, want 1", len(bodies))
		}
		if _, ok := bodies[0]["previous_response_id"]; !ok {
			t.Errorf("compacted from %v, want the pinned chain", bodies[0])
		}
	})

	// Past the window the conflict is real and permanent: the pinned mode
	// rewrites from a chain that never carried the oldest entries, and only the
	// caller can resolve it (by unpinning the mode or dropping the window). The
	// pass is abandoned every run, which is the deliberate trade — a rewrite
	// that deleted them unread is the alternative.
	t.Run("log past the window", func(t *testing.T) {
		if bodies := windowedRun(t, 6, 2); len(bodies) != 0 {
			t.Errorf("compact calls = %d, want 0 — the chain never carried the pre-window entries", len(bodies))
		}
	})
}

func mustInput(t *testing.T, raw string) agents.InputItem {
	t.Helper()
	item, err := session.UnmarshalInputItem([]byte(raw))
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

func newCompactStub(t *testing.T) *httptest.Server {
	t.Helper()
	return newCompactStubDelayed(t, nil)
}

// newCompactStubDelayed is newCompactStub with a hook that runs while the
// compact call is in flight — the window a concurrent append lands in.
func newCompactStubDelayed(t *testing.T, inFlight func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if inFlight != nil {
			inFlight()
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

// newCompactionSession wraps under without pointing it at a compact endpoint,
// for the paths that never reach one.
func newCompactionSession(t *testing.T, under session.Storage) *CompactionSession {
	t.Helper()
	sess, err := NewCompactionSession(under, CompactionOptions{Model: "gpt-4.1"}, option.WithAPIKey("test"))
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func seededCompactionSession(t *testing.T, baseURL string, n int, under session.Storage) *CompactionSession {
	t.Helper()
	ctx := t.Context()
	seed := make([]agents.InputItem, 0, n)
	for range n {
		seed = append(seed, mustInput(t, `{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}`))
	}
	if err := session.NewSession(under).AppendItems(ctx, seed, agents.Source{}); err != nil {
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
	sess := seededCompactionSession(t, srv.URL, 12, session.NewInMemorySession())

	span := &tracing.SpanHandle{Span: &tracing.Span{Data: map[string]any{}}}
	startCalls := 0
	args := session.CompactionArgs{
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
	sess := seededCompactionSession(t, srv.URL, 1, session.NewInMemorySession())

	startCalls := 0
	args := session.CompactionArgs{
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
	under := session.NewInMemoryStorage("test")
	sess := seededCompactionSession(t, srv.URL, 12, under)

	if err := sess.RunCompaction(ctx, session.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}
	items, err := session.NewSession(under).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("after compaction len = %d, want 1", len(items))
	}
}

// --- CompactionSession atomic history replacement --------------------------

// replaceRecordingSession wraps InMemoryStorage, counting the write paths a
// history rewrite can take so a test can say which one compaction used.
type replaceRecordingSession struct {
	*session.InMemoryStorage
	mu           sync.Mutex
	clearCalls   int
	replaceCalls int
	guardedCalls int
}

func (r *replaceRecordingSession) Clear(ctx context.Context) error {
	r.mu.Lock()
	r.clearCalls++
	r.mu.Unlock()
	return r.InMemoryStorage.Clear(ctx)
}

func (r *replaceRecordingSession) ReplaceEntries(ctx context.Context, entries ...session.Entry) error {
	r.mu.Lock()
	r.replaceCalls++
	r.mu.Unlock()
	return r.InMemoryStorage.ReplaceEntries(ctx, entries...)
}

func (r *replaceRecordingSession) ReplaceEntriesIf(ctx context.Context, expect int64, entries ...session.Entry) (bool, error) {
	r.mu.Lock()
	r.guardedCalls++
	r.mu.Unlock()
	return r.InMemoryStorage.ReplaceEntriesIf(ctx, expect, entries...)
}

func (r *replaceRecordingSession) counts() (cleared, replaced, guarded int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clearCalls, r.replaceCalls, r.guardedCalls
}

// atomicOnlyStorage is what a third-party store looks like from RunCompaction's
// side: it can swap a history atomically but cannot say whether the log moved.
// Embedding the interface hides the guarded method the in-memory store has.
type atomicOnlyStorage struct {
	session.Storage
	mu           sync.Mutex
	replaceCalls int
}

func (a *atomicOnlyStorage) ReplaceEntries(ctx context.Context, entries ...session.Entry) error {
	a.mu.Lock()
	a.replaceCalls++
	a.mu.Unlock()
	return a.Storage.(session.AtomicReplacer).ReplaceEntries(ctx, entries...)
}

func TestRunCompactionUsesGuardedReplace(t *testing.T) {
	ctx := t.Context()
	srv := newCompactStub(t)
	under := &replaceRecordingSession{InMemoryStorage: session.NewInMemoryStorage("test")}
	sess := seededCompactionSession(t, srv.URL, 12, under)

	if err := sess.RunCompaction(ctx, session.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}

	clearCalls, replaceCalls, guardedCalls := under.counts()
	if guardedCalls != 1 {
		t.Errorf("ReplaceEntriesIf calls = %d, want 1", guardedCalls)
	}
	if replaceCalls != 0 {
		t.Errorf("ReplaceEntries calls = %d, want 0 (a store that can compare the log back is asked to)", replaceCalls)
	}
	if clearCalls != 0 {
		t.Errorf("Clear calls = %d, want 0 (history must not go through a clear+add window)", clearCalls)
	}

	items, err := session.NewSession(under).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("after compaction len = %d, want 1", len(items))
	}
	b, _ := session.MarshalInputItem(items[0])
	if !contains(string(b), "summary") {
		t.Errorf("compacted item = %s", b)
	}
}

// A store without the guard still gets compacted, through the atomic swap.
func TestRunCompactionFallsBackToAtomicReplace(t *testing.T) {
	ctx := t.Context()
	srv := newCompactStub(t)
	under := &atomicOnlyStorage{Storage: session.NewInMemoryStorage("test")}
	sess := seededCompactionSession(t, srv.URL, 12, under)

	if err := sess.RunCompaction(ctx, session.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}

	under.mu.Lock()
	replaceCalls := under.replaceCalls
	under.mu.Unlock()
	if replaceCalls != 1 {
		t.Errorf("ReplaceEntries calls = %d, want 1", replaceCalls)
	}
	items, err := session.NewSession(under).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("after compaction len = %d, want 1", len(items))
	}
}

// The window the guard exists for: a turn is persisted while responses.compact
// is in flight. Its entry is not in the replacement, so applying the pass would
// delete it — the pass is abandoned instead.
func TestRunCompactionAbandonsPassOnConcurrentAppend(t *testing.T) {
	ctx := t.Context()
	under := session.NewInMemoryStorage("test")

	inFlight := make(chan struct{})
	appended := make(chan struct{})
	srv := newCompactStubDelayed(t, func() {
		close(inFlight)
		<-appended
	})
	sess := seededCompactionSession(t, srv.URL, 12, under)

	span := &tracing.SpanHandle{Span: &tracing.Span{Data: map[string]any{}}}
	done := make(chan error, 1)
	go func() {
		done <- sess.RunCompaction(ctx, session.CompactionArgs{
			ResponseID: "resp_1",
			StartSpan:  func() *tracing.SpanHandle { return span },
		})
	}()

	<-inFlight
	if err := session.NewSession(under).AppendItems(ctx,
		[]agents.InputItem{mustInput(t, `{"role":"user","content":"landed mid-flight"}`)},
		agents.Source{}); err != nil {
		t.Fatal(err)
	}
	close(appended)
	if err := <-done; err != nil {
		t.Fatalf("an abandoned pass is not a failure: %v", err)
	}

	items, err := session.NewSession(under).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 13 {
		t.Fatalf("history holds %d items, want the 12 seeded plus the one appended mid-flight", len(items))
	}
	b, _ := session.MarshalInputItem(items[12])
	if !contains(string(b), "landed mid-flight") {
		t.Errorf("the entry appended mid-flight did not survive the pass; last item = %s", b)
	}
	if got := span.Span.Data["abandoned"]; got != "concurrent_append" {
		t.Errorf("span abandoned = %v, want the pass to record why it dropped", got)
	}
}

// Wrapping a session in compaction does not take a capability away from it: the
// guard reaches the wrapped store when it has one. A store without it gets an
// error, never an unguarded rewrite — the caller asserted the interface to have
// the log compared back, and "replaced=false" would claim it was.
func TestCompactionSessionDelegatesGuardedReplace(t *testing.T) {
	ctx := t.Context()
	under := &replaceRecordingSession{InMemoryStorage: session.NewInMemoryStorage("test")}
	sess := newCompactionSession(t, under)

	entry, err := session.NewItemEntry(mustInput(t, `{"role":"user","content":"only"}`), agents.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if err := under.Append(ctx, entry); err != nil {
		t.Fatal(err)
	}
	stored, err := under.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	expect := stored[0].Seq

	replaced, err := sess.ReplaceEntriesIf(ctx, expect, entry)
	if err != nil {
		t.Fatalf("guarded replace through the wrapper: %v", err)
	}
	if !replaced {
		t.Fatalf("the wrapper refused Seq %d, which is where the log stands", expect)
	}
	if _, _, guarded := under.counts(); guarded != 1 {
		t.Errorf("ReplaceEntriesIf calls on the wrapped store = %d, want 1", guarded)
	}

	// The guard is the wrapped store's answer, not the wrapper's: expect is now
	// a sequence number the log has moved past.
	replaced, err = sess.ReplaceEntriesIf(ctx, expect, entry)
	if err != nil {
		t.Fatalf("guarded replace against a stale seq: %v", err)
	}
	if replaced {
		t.Errorf("the wrapper wrote against Seq %d after the log moved past it", expect)
	}

	plain := newCompactionSession(t, &atomicOnlyStorage{Storage: session.NewInMemoryStorage("test")})
	if _, err := plain.ReplaceEntriesIf(ctx, 0, entry); err == nil {
		t.Error("a store without the guard was rewritten anyway")
	}
}

// An update entry names its target by id, and both travel through the rewrite.
// Re-minting ids on the way would leave the update pointing at nothing, and a
// fold that finds no target is silently dropped — the late display it carries
// (a background task's card) lost for good.
func TestRunCompactionKeepsUpdateLinks(t *testing.T) {
	ctx := t.Context()
	srv := newCompactStub(t)
	under := session.NewInMemoryStorage("test")
	sess := seededCompactionSession(t, srv.URL, 12, under)

	if err := under.Append(ctx, session.NewAnnotationEntry(
		agents.ItemDisplay{Kind: agents.DisplayToolCall, Text: "task started"},
		agents.Source{Type: agents.SourceTool},
	)); err != nil {
		t.Fatal(err)
	}
	stored, err := under.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	target := stored[len(stored)-1].ID
	upd, err := session.NewUpdateEntry(target, agents.ItemDisplay{Text: "task finished"})
	if err != nil {
		t.Fatal(err)
	}
	if err := under.Append(ctx, upd); err != nil {
		t.Fatal(err)
	}

	if err := sess.RunCompaction(ctx, session.CompactionArgs{ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}

	after, err := under.Entries(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	folded := false
	for _, e := range session.FoldUpdates(after) {
		if e.Kind == session.EntryKindAnnotation && e.Display != nil && e.Display.Text == "task finished" {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("the update no longer reaches its target after compaction; entries = %+v", after)
	}
}
