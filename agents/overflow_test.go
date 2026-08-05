package agents

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/session"
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

// scriptedOverflowModel answers turn by turn from a script, a nil step failing
// with a context-length error. overflowingModel above covers "fail the first n
// calls, then answer"; this one is for a run whose overflow lands mid-script,
// and it keeps every request so a test can look at what the RETRY was sent.
type scriptedOverflowModel struct {
	steps []*ModelResponse
	calls int
	reqs  []ModelRequest
}

func (m *scriptedOverflowModel) next(req ModelRequest) (*ModelResponse, error) {
	m.reqs = append(m.reqs, req)
	i := m.calls
	m.calls++
	if i >= len(m.steps) || m.steps[i] == nil {
		return nil, errors.New("400 Bad Request: This model's maximum context length is 128000 tokens")
	}
	return m.steps[i], nil
}

func (m *scriptedOverflowModel) Respond(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	return m.next(req)
}

func (m *scriptedOverflowModel) StreamResponse(_ context.Context, req ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(yield func(*ResponseStreamEvent, error) bool) {
		resp, err := m.next(req)
		if err != nil {
			yield(nil, err)
			return
		}
		event := completedStreamEvent(resp)
		yield(&event, nil)
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

// A steer taken at the save point is not yet in the log: the save point writes
// first and drains the queue after. Overflow recovery rebuilds from the log and
// throws the in-flight items away, so recovering without flushing first hands
// the model a conversation the caller's words never reached — while the next
// write past their mark still counts them delivered.
func TestOverflow_RetryKeepsInjectedInput(t *testing.T) {
	const steer = "and check the date"
	sess := seededSession(t, "old")
	tool := NewTool("probe", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "result", nil
	})
	model := &scriptedOverflowModel{steps: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		nil, // the turn the steer rides on overflows
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	stream, ctrl := Run(context.Background(), agent, "go", RunOptions{
		Conversation: ConversationOptions{Session: sess},
		Compaction:   CompactionOptions{Compactor: &recordingCompactor{drop: 1}, Points: CompactAtSavePoint},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 2}},
	})
	var res *RunResult
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		// Said while the turn is still running, so the save point drains it into
		// the turn that is about to overflow.
		if it, ok := ev.(*RunItemStreamEvent); ok && it.Item.Kind == ItemToolCall {
			if serr := ctrl.Steer(steer); serr != nil {
				t.Fatal(serr)
			}
		}
		if done, ok := ev.(*RunCompletedEvent); ok {
			res = done.Result
		}
	}
	if res == nil || res.FinalOutputString() != "done" {
		t.Fatalf("the run did not survive the overflow: %+v", res)
	}
	if len(model.reqs) != 3 {
		t.Fatalf("model calls = %d, want 3 (the turn, its overflow, the retry)", len(model.reqs))
	}
	if got := inputTexts(model.reqs[2].Input); !strings.Contains(got, steer) {
		t.Errorf("the retry lost the steer: %s", got)
	}
	// Delivered has to mean delivered: nothing left queued, and the log holds it
	// once rather than twice.
	if !ctrl.Pending().Empty() {
		t.Errorf("the delivered steer is still queued: %+v", ctrl.Pending())
	}
	entries, err := sess.Entries(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, e := range entries {
		if in, ierr := e.InputItem(); ierr == nil && session.ItemText(in) == steer {
			stored++
		}
	}
	if stored != 1 {
		t.Errorf("the steer is in the log %d times, want 1", stored)
	}
}

// selfCompactingStorage is a session.CompactionAware storage whose forced pass drops
// the oldest `drop` entries. Non-forced calls (after-run housekeeping) are
// recorded but do nothing, so the test isolates the overflow path.
type selfCompactingStorage struct {
	session.Storage
	drop        int
	forcedCalls int
	normalCalls int
}

func (s *selfCompactingStorage) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	if !args.Force {
		s.normalCalls++
		return nil
	}
	s.forcedCalls++
	if s.drop <= 0 {
		return nil
	}
	entries, err := s.Entries(ctx, session.Cursor{})
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
	st := &selfCompactingStorage{Storage: session.NewInMemoryStorage("test"), drop: 2}
	items := InputItemsFromText("one")
	items = append(items, InputItemsFromText("two")...)
	items = append(items, InputItemsFromText("three")...)
	entries, err := session.NewItemEntries(items, Source{Type: SourceUser})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(context.Background(), entries...); err != nil {
		t.Fatal(err)
	}

	model := &overflowingModel{failures: 1, answer: modelResp(messageOutput(t, "recovered"))}
	agent := &Agent{Name: "a", ModelImpl: model}
	res, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(st)},
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
	st := &selfCompactingStorage{Storage: session.NewInMemoryStorage("test"), drop: 0}
	if err := st.Append(context.Background(), mustItemEntries(t, "one")...); err != nil {
		t.Fatal(err)
	}
	model := &overflowingModel{failures: 5}
	agent := &Agent{Name: "a", ModelImpl: model}
	_, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(st)},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 3}},
	})
	if err == nil {
		t.Fatal("expected the overflow to be reported")
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 — a no-op forced pass must not buy a retry", model.calls)
	}
}

// abandoningStorage models the guarded-replace loser: something appended while
// the forced pass was in flight, so the replacement is refused and the log ends
// the pass longer than it started rather than shorter.
type abandoningStorage struct {
	session.Storage
}

func (s *abandoningStorage) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	if !args.Force {
		return nil
	}
	entries, err := session.NewItemEntries(InputItemsFromText("arrived mid-pass"), Source{Type: SourceUser})
	if err != nil {
		return err
	}
	return s.Append(ctx, entries...)
}

// A pass that was abandoned still leaves the history DIFFERENT from what it
// started with — the very append that made it abandon saw to that. Difference
// alone must not buy a retry of a context that only grew.
func TestOverflow_StorageAbandonedPassBuysNoRetry(t *testing.T) {
	st := &abandoningStorage{Storage: session.NewInMemoryStorage("test")}
	if err := st.Append(context.Background(), mustItemEntries(t, "one")...); err != nil {
		t.Fatal(err)
	}
	model := &overflowingModel{failures: 5}
	agent := &Agent{Name: "a", ModelImpl: model}
	_, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(st)},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 3}},
	})
	if err == nil {
		t.Fatal("expected the overflow to be reported")
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 — an abandoned pass must not buy a retry", model.calls)
	}
}

// summarizingStorage's forced pass swaps every entry for a shorter summary and
// leaves the COUNT alone — one summary standing in for one entry, which is a
// legal pass.
type summarizingStorage struct {
	session.Storage
}

func (s *summarizingStorage) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	if !args.Force {
		return nil
	}
	entries, err := s.Entries(ctx, session.Cursor{})
	if err != nil {
		return err
	}
	summaries := make([]InputItem, 0, len(entries))
	for range entries {
		summaries = append(summaries, InputItemsFromText("summary")...)
	}
	replacement, err := session.NewItemEntries(summaries, Source{Type: SourceUser})
	if err != nil {
		return err
	}
	if err := s.Clear(ctx); err != nil {
		return err
	}
	return s.Append(ctx, replacement...)
}

// Length is not the test on this path either: a pass that returns the same
// COUNT with shorter content really did shrink the context, and reading that as
// a no-op would throw away a recovery the storage just performed.
func TestOverflow_StorageSameCountSummaryBuysRetry(t *testing.T) {
	st := &summarizingStorage{Storage: session.NewInMemoryStorage("test")}
	if err := st.Append(context.Background(), mustItemEntries(t, "one", "two")...); err != nil {
		t.Fatal(err)
	}
	model := &overflowingModel{failures: 1, answer: modelResp(messageOutput(t, "recovered"))}
	agent := &Agent{Name: "a", ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: session.NewSession(st)},
		Exec:         ExecOptions{Overflow: OverflowPolicy{MaxRetries: 2}},
	})
	if err != nil {
		t.Fatalf("a same-count summary pass was read as a no-op: %v", err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2 (the overflow, then the retry)", model.calls)
	}
	if got := inputTexts(model.lastReq.Input); !strings.Contains(got, "summary") {
		t.Errorf("the retry was not rebuilt from the compacted history: %s", got)
	}
}

func mustItemEntries(t *testing.T, texts ...string) []session.Entry {
	t.Helper()
	items := make([]InputItem, 0, len(texts))
	for _, text := range texts {
		items = append(items, InputItemsFromText(text)...)
	}
	entries, err := session.NewItemEntries(items, Source{Type: SourceUser})
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
