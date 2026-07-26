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

func (m *overflowingModel) GetResponse(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.calls++
	m.lastReq = req
	if m.calls <= m.failures {
		return nil, errors.New("400 Bad Request: This model's maximum context length is 128000 tokens")
	}
	return m.answer, nil
}

func (m *overflowingModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
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
		{errors.New("400: Invalid value for 'temperature'"), false},
		{errors.New("429 rate limit"), false},
		{nil, false},
	} {
		if got := DetectContextOverflow(tc.err, nil); got != tc.want {
			t.Errorf("DetectContextOverflow(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
	// A truncated response is a different problem with a different fix: its
	// input fit, and compacting the input does not raise the output cap.
	truncated := &ModelResponse{Status: "incomplete", IncompleteReason: "max_output_tokens"}
	if DetectContextOverflow(nil, truncated) {
		t.Error("a truncated response was classified as a context overflow")
	}
}
