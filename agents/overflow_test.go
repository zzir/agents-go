package agents

import (
	"context"
	"errors"
	"iter"
	"testing"
)

// overflowingModel fails the first n calls with a context-length error.
type overflowingModel struct {
	failures int
	calls    int
	answer   *ModelResponse
	lastReq  ModelRequest
}

func (m *overflowingModel) Respond(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.calls++
	m.lastReq = req
	if m.calls <= m.failures {
		return nil, errors.New("400 Bad Request: This model's maximum context length is 128000 tokens")
	}
	return m.answer, nil
}

func (m *overflowingModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(yield func(*ResponseStreamEvent, error) bool) {
		yield(nil, errors.New("not used"))
	}
}

// Compaction predicts; overflow recovery reacts. The prediction is an estimate
// against a window the provider never states exactly, so it will sometimes be
// wrong — and the failure it misses is one the run cannot otherwise survive.
func TestOverflow_CompactsAndRetriesTheTurn(t *testing.T) {
	c := &recordingCompactor{drop: 2}
	model := &overflowingModel{failures: 1, answer: modelResp(messageOutput(t, "recovered"))}
	agent := &Agent{Name: "a", ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "one", "two", "three")},
		Compaction:   CompactionOptions{Compactor: c, Points: CompactAtSavePoint},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 2}},
	})
	if err != nil {
		t.Fatalf("the run did not survive an overflow: %v", err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2 (the overflow, then the retry)", model.calls)
	}
	// The retry sent a shorter context than the attempt that overflowed.
	if len(model.lastReq.Input) >= 4 {
		t.Errorf("the retry sent %d items; compaction did not shrink it", len(model.lastReq.Input))
	}
	// A run that recovered from an overflow still says so.
	found := false
	for _, d := range res.Diagnostics {
		if d.Type == DiagContextOverflow {
			found = true
		}
	}
	if !found {
		t.Error("the overflow was not recorded as a diagnostic")
	}
}

// The turn budget counts model calls the model made, and an overflow is one it
// never got.
func TestOverflow_RetryDoesNotSpendTheTurnBudget(t *testing.T) {
	c := &recordingCompactor{drop: 1}
	model := &overflowingModel{failures: 1, answer: modelResp(messageOutput(t, "ok"))}
	agent := &Agent{Name: "a", ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "one", "two")},
		Compaction:   CompactionOptions{Compactor: c, Points: CompactAtSavePoint},
		// One turn: the overflow retry must not consume it.
		Exec: ExecOptions{MaxTurns: 1, Overflow: OverflowPolicy{MaxRetries: 2}},
	}); err != nil {
		t.Fatalf("the overflow retry consumed the turn budget: %v", err)
	}
}

// Off by default: an overflow is reported rather than silently shrinking the
// conversation.
func TestOverflow_DisabledByDefault(t *testing.T) {
	model := &overflowingModel{failures: 1, answer: modelResp(messageOutput(t, "never"))}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "one")},
		Compaction:   CompactionOptions{Compactor: &recordingCompactor{drop: 1}},
	})
	if err == nil {
		t.Fatal("the overflow was silently absorbed")
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 — no retry without a policy", model.calls)
	}
}

// Retrying an identical request would fail identically, and spending the budget
// on that is worse than reporting the overflow.
func TestOverflow_NoRetryWhenCompactionChangesNothing(t *testing.T) {
	// drop: 0 — the compactor returns what it was given.
	c := &recordingCompactor{}
	model := &overflowingModel{failures: 5}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "one")},
		Compaction:   CompactionOptions{Compactor: c, Points: CompactAtSavePoint},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 3}},
	})
	if err == nil {
		t.Fatal("expected the overflow to be reported")
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 — a no-op compaction must not buy a retry", model.calls)
	}
}

// selfCompactingStorage is a CompactionAware storage whose forced pass drops
// the oldest `drop` entries. Non-forced calls (after-run housekeeping) are
// recorded but do nothing, so the test isolates the overflow path.
type selfCompactingStorage struct {
	SessionStorage
	drop        int
	forcedCalls int
	normalCalls int
}

func (s *selfCompactingStorage) RunCompaction(ctx context.Context, args CompactionArgs) error {
	if !args.Force {
		s.normalCalls++
		return nil
	}
	s.forcedCalls++
	if s.drop <= 0 {
		return nil
	}
	entries, err := s.Entries(ctx, Cursor{})
	if err != nil {
		return err
	}
	if len(entries) <= s.drop {
		return nil
	}
	if err := s.Clear(ctx); err != nil {
		return err
	}
	return s.Append(ctx, entries[s.drop:]...)
}

// A self-compacting storage (a server-side compact API) has no run-level
// Compactor, but an overflow still recovers: the storage gets a FORCED pass —
// its own trigger already answered "not yet", and the provider just overruled
// it — and the turn retries from the rebuilt, smaller context.
func TestOverflow_ForcesSelfCompactingStorage(t *testing.T) {
	st := &selfCompactingStorage{SessionStorage: NewInMemoryStorage("test"), drop: 2}
	items := InputItemsFromText("one")
	items = append(items, InputItemsFromText("two")...)
	items = append(items, InputItemsFromText("three")...)
	entries, err := NewItemEntries(items, Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(context.Background(), entries...); err != nil {
		t.Fatal(err)
	}

	model := &overflowingModel{failures: 1, answer: modelResp(messageOutput(t, "recovered"))}
	agent := &Agent{Name: "a", ModelImpl: model}
	res, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: NewSession(st)},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 2}},
	})
	if err != nil {
		t.Fatalf("the run did not survive an overflow: %v", err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if st.forcedCalls != 1 {
		t.Errorf("forced passes = %d, want 1", st.forcedCalls)
	}
	// The retry sent a shorter context than the attempt that overflowed.
	if len(model.lastReq.Input) >= 4 {
		t.Errorf("the retry sent %d items; the forced pass did not shrink it", len(model.lastReq.Input))
	}
}

// A forced pass that changes nothing buys no retry — same rule as the
// Compactor path.
func TestOverflow_StorageNoopBuysNoRetry(t *testing.T) {
	st := &selfCompactingStorage{SessionStorage: NewInMemoryStorage("test"), drop: 0}
	if err := st.Append(context.Background(), mustItemEntries(t, "one")...); err != nil {
		t.Fatal(err)
	}
	model := &overflowingModel{failures: 5}
	agent := &Agent{Name: "a", ModelImpl: model}
	_, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: NewSession(st)},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 3}},
	})
	if err == nil {
		t.Fatal("expected the overflow to be reported")
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 — a no-op forced pass must not buy a retry", model.calls)
	}
}

func mustItemEntries(t *testing.T, texts ...string) []SessionEntry {
	t.Helper()
	items := make([]InputItem, 0, len(texts))
	for _, text := range texts {
		items = append(items, InputItemsFromText(text)...)
	}
	entries, err := NewItemEntries(items, Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// Treating every 400 as an overflow would compact and retry after a malformed
// request, hiding a bug behind a shrinking conversation.
func TestDetectContextOverflow(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("400: This model's maximum context length is 128000 tokens"), true},
		{errors.New("context_length_exceeded"), true},
		{errors.New("input exceeds the context window"), true},
		{errors.New("400: prompt is too long: 210000 tokens > 200000 maximum"), true},
		{errors.New("anthropic: response stopped: model_context_window_exceeded"), true},
		{errors.New("400: Invalid value for 'temperature'"), false},
		{errors.New("429 rate limit"), false},
		{nil, false},
	} {
		if got := DetectContextOverflow(tc.err); got != tc.want {
			t.Errorf("DetectContextOverflow(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
