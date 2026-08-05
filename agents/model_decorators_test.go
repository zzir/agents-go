package agents

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

// scriptedModel returns a queued (resp, err) per GetResponse call. Its
// StreamResponse is a no-op present only to satisfy the Model interface.
type scriptedModel struct {
	steps []scriptStep
	calls int
}

type scriptStep struct {
	resp *ModelResponse
	err  error
}

func (m *scriptedModel) GetResponse(_ context.Context, _ ModelRequest) (*ModelResponse, error) {
	i := m.calls
	m.calls++
	if i >= len(m.steps) {
		return &ModelResponse{}, nil
	}
	return m.steps[i].resp, m.steps[i].err
}

func (m *scriptedModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(func(*TResponseStreamEvent, error) bool) {}
}

// streamStep emits `events` placeholder events, then errors with `err` (nil = clean finish).
type streamStep struct {
	events int
	err    error
}

type scriptedStreamModel struct {
	steps []streamStep
	calls int
}

func (m *scriptedStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return &ModelResponse{}, nil
}

func (m *scriptedStreamModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		i := m.calls
		m.calls++
		var st streamStep
		if i < len(m.steps) {
			st = m.steps[i]
		}
		for range st.events {
			if !yield(&TResponseStreamEvent{}, nil) {
				return
			}
		}
		if st.err != nil {
			yield(nil, st.err)
		}
	}
}

// drain consumes a stream, counting events and returning the terminal error.
func drain(seq iter.Seq2[*TResponseStreamEvent, error]) (int, error) {
	count := 0
	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
		count++
	}
	return count, gotErr
}

// noSleepPolicy makes retry backoff instant in tests.
func noSleepPolicy(p RetryPolicy) RetryPolicy {
	p.sleep = func(context.Context, time.Duration) error { return nil }
	return p
}

func TestRetryModel_SucceedsAfterFailures(t *testing.T) {
	inner := &scriptedModel{steps: []scriptStep{
		{nil, errBoom}, {nil, errBoom}, {&ModelResponse{ResponseID: "ok"}, nil},
	}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 3}))
	resp, err := m.GetResponse(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp.ResponseID != "ok" {
		t.Errorf("ResponseID = %q", resp.ResponseID)
	}
	if inner.calls != 3 {
		t.Errorf("calls = %d, want 3", inner.calls)
	}
}

func TestRetryModel_ExhaustsAndReturnsLastError(t *testing.T) {
	inner := &scriptedModel{steps: []scriptStep{{nil, errBoom}, {nil, errBoom}, {nil, errBoom}}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 3}))
	_, err := m.GetResponse(context.Background(), ModelRequest{})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if inner.calls != 3 {
		t.Errorf("calls = %d, want 3", inner.calls)
	}
}

func TestRetryModel_NonRetryableStopsImmediately(t *testing.T) {
	inner := &scriptedModel{steps: []scriptStep{{nil, errBoom}, {&ModelResponse{}, nil}}}
	p := noSleepPolicy(RetryPolicy{MaxAttempts: 5, RetryIf: func(error) bool { return false }})
	m := NewRetryModel(inner, p)
	_, err := m.GetResponse(context.Background(), ModelRequest{})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if inner.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", inner.calls)
	}
}

func TestRetryModel_DefaultRetryIfSkipsContextCancel(t *testing.T) {
	inner := &scriptedModel{steps: []scriptStep{{nil, context.Canceled}}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 5}))
	_, err := m.GetResponse(context.Background(), ModelRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("calls = %d, want 1", inner.calls)
	}
}

func TestRetryModel_StreamRetriesBeforeFirstEvent(t *testing.T) {
	inner := &scriptedStreamModel{steps: []streamStep{{0, errBoom}, {2, nil}}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 3}))
	events, gotErr := drain(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	if events != 2 {
		t.Errorf("events = %d, want 2", events)
	}
	if inner.calls != 2 {
		t.Errorf("calls = %d, want 2", inner.calls)
	}
}

func TestRetryModel_StreamDoesNotRetryAfterEmitting(t *testing.T) {
	inner := &scriptedStreamModel{steps: []streamStep{{1, errBoom}, {3, nil}}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 3}))
	events, gotErr := drain(m.StreamResponse(context.Background(), ModelRequest{}))
	if !errors.Is(gotErr, errBoom) {
		t.Fatalf("err = %v, want errBoom", gotErr)
	}
	if events != 1 {
		t.Errorf("events = %d, want 1 (committed after first event)", events)
	}
	if inner.calls != 1 {
		t.Errorf("calls = %d, want 1", inner.calls)
	}
}

// typedStreamModel yields events with the given types per attempt, then errors
// (nil = clean finish) — for tests that exercise the lifecycle/output split.
type typedStreamModel struct {
	steps []typedStreamStep
	calls int
}

type typedStreamStep struct {
	types []string
	err   error
}

func (m *typedStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return &ModelResponse{}, nil
}

func (m *typedStreamModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		i := m.calls
		m.calls++
		var st typedStreamStep
		if i < len(m.steps) {
			st = m.steps[i]
		}
		for _, typ := range st.types {
			if !yield(&TResponseStreamEvent{Type: typ}, nil) {
				return
			}
		}
		if st.err != nil {
			yield(nil, st.err)
		}
	}
}

// drainTypes consumes a stream, returning the event types seen in order and
// the terminal error.
func drainTypes(seq iter.Seq2[*TResponseStreamEvent, error]) ([]string, error) {
	var types []string
	var gotErr error
	for ev, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
		types = append(types, ev.Type)
	}
	return types, gotErr
}

func TestRetryModel_StreamRetriesAfterLifecyclePreamble(t *testing.T) {
	// The stream died after response.created/in_progress — nothing generated
	// yet, so the attempt is retried. The abandoned attempt's preamble was
	// held back and dropped: the consumer sees ONE coherent response, not two
	// response.created.
	inner := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created", "response.in_progress"}, err: errBoom},
		{types: []string{"response.created", "response.output_text.delta"}},
	}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 3}))
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	want := []string{"response.created", "response.output_text.delta"}
	if !slices.Equal(types, want) {
		t.Errorf("types = %v, want %v (abandoned preamble dropped)", types, want)
	}
	if inner.calls != 2 {
		t.Errorf("calls = %d, want 2", inner.calls)
	}
}

func TestRetryModel_StreamDoesNotRetryAfterOutputEvent(t *testing.T) {
	inner := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created", "response.output_item.added"}, err: errBoom},
		{types: []string{"response.created"}},
	}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 3}))
	events, gotErr := drain(m.StreamResponse(context.Background(), ModelRequest{}))
	if !errors.Is(gotErr, errBoom) {
		t.Fatalf("err = %v, want errBoom", gotErr)
	}
	if events != 2 || inner.calls != 1 {
		t.Errorf("events=%d calls=%d, want 2/1 (committed after output)", events, inner.calls)
	}
}

func TestRetryModel_StreamFlushesPreambleOnTerminalFailure(t *testing.T) {
	// Every attempt died during the preamble. The last attempt's preamble is
	// the stream's final word: delivered ahead of the error, so the consumer
	// sees what arrived rather than nothing.
	inner := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created"}, err: errBoom},
		{types: []string{"response.created", "response.in_progress"}, err: errBoom},
	}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 2}))
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if !errors.Is(gotErr, errBoom) {
		t.Fatalf("err = %v, want errBoom", gotErr)
	}
	want := []string{"response.created", "response.in_progress"}
	if !slices.Equal(types, want) {
		t.Errorf("types = %v, want %v (last attempt's preamble only)", types, want)
	}
}

func TestRetryModel_StreamFlushesPreambleOnCleanAllPreambleFinish(t *testing.T) {
	// A stream that finishes cleanly having produced only preamble still
	// delivers it — held-back events must not vanish on success.
	inner := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created"}},
	}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 3}))
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	if !slices.Equal(types, []string{"response.created"}) {
		t.Errorf("types = %v, want [response.created]", types)
	}
}

// nilEventStreamModel yields (nil, nil) then one output event — the input that
// used to panic the decorators' ev.Type access (the runner tolerates nil, so
// the decorators must too).
type nilEventStreamModel struct{ calls int }

func (m *nilEventStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return &ModelResponse{}, nil
}

func (m *nilEventStreamModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		m.calls++
		if !yield(nil, nil) {
			return
		}
		yield(&TResponseStreamEvent{Type: "response.output_text.delta"}, nil)
	}
}

func TestRetryModel_StreamToleratesNilEvent(t *testing.T) {
	inner := &nilEventStreamModel{}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 2}))
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	if !slices.Equal(types, []string{"response.output_text.delta"}) {
		t.Errorf("types = %v, want the output event only (nil dropped)", types)
	}
}

func TestFallbackModel_StreamToleratesNilEvent(t *testing.T) {
	inner := &nilEventStreamModel{}
	m := NewFallbackModel(inner)
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	if !slices.Equal(types, []string{"response.output_text.delta"}) {
		t.Errorf("types = %v, want the output event only (nil dropped)", types)
	}
}

func TestRetryModel_StreamRetriesAfterTerminalFailureEvent(t *testing.T) {
	// response.failed carries no model output — replacing an attempt that ends
	// in it is the whole point of retrying. The failed attempt's events
	// (preamble AND the failure event) are dropped.
	inner := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created", "response.failed"}, err: errBoom},
		{types: []string{"response.created", "response.output_text.delta"}},
	}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 2}))
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	want := []string{"response.created", "response.output_text.delta"}
	if !slices.Equal(types, want) {
		t.Errorf("types = %v, want %v (failed attempt dropped)", types, want)
	}
	if inner.calls != 2 {
		t.Errorf("calls = %d, want 2", inner.calls)
	}
}

func TestRetryModel_StreamFlushesFailureEventOnTerminalFailure(t *testing.T) {
	// No retry follows: the attempt's held-back events — preamble and the
	// failure event — are the stream's last word, delivered ahead of the error.
	inner := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created", "response.failed"}, err: errBoom},
	}}
	m := NewRetryModel(inner, noSleepPolicy(RetryPolicy{MaxAttempts: 1}))
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if !errors.Is(gotErr, errBoom) {
		t.Fatalf("err = %v, want errBoom", gotErr)
	}
	want := []string{"response.created", "response.failed"}
	if !slices.Equal(types, want) {
		t.Errorf("types = %v, want %v", types, want)
	}
}

func TestFallbackModel_StreamAdvancesOnTerminalFailureEvent(t *testing.T) {
	// The streaming chain must advance on a response.failed like the blocking
	// chain advances on the error it becomes — same failure, same fallback.
	primary := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created", "response.failed"}, err: errBoom},
	}}
	backup := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created", "response.output_text.delta"}},
	}}
	m := NewFallbackModel(primary, backup)
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	want := []string{"response.created", "response.output_text.delta"}
	if !slices.Equal(types, want) {
		t.Errorf("types = %v, want %v (failed backend dropped)", types, want)
	}
	if backup.calls != 1 {
		t.Errorf("backup.calls = %d, want 1", backup.calls)
	}
}

func TestFallbackModel_StreamNoDiagnosticWhenConsumerStopsMidFlush(t *testing.T) {
	// The backup finishes cleanly with only held-back events; the consumer
	// stops during the flush. An abandoned run records no fallback diagnostic.
	primary := &typedStreamModel{steps: []typedStreamStep{{err: errBoom}}}
	backup := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created"}},
	}}
	sink := &DiagnosticSink{}
	ctx := WithDiagnostics(context.Background(), sink)
	for range NewFallbackModel(primary, backup).StreamResponse(ctx, ModelRequest{}) {
		break
	}
	if ds, _ := sink.TakeSince(0); len(ds) != 0 {
		t.Fatalf("diagnostics = %v, want none (consumer abandoned the run)", ds)
	}
}

func TestFallbackModel_StreamFallsBackAfterLifecyclePreamble(t *testing.T) {
	// Same rule as retry: preamble does not commit the backend. The abandoned
	// primary's preamble is dropped, and the consumer sees only the backup's
	// single coherent response.
	primary := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created"}, err: errBoom},
	}}
	backup := &typedStreamModel{steps: []typedStreamStep{
		{types: []string{"response.created", "response.output_text.delta"}},
	}}
	m := NewFallbackModel(primary, backup)
	types, gotErr := drainTypes(m.StreamResponse(context.Background(), ModelRequest{}))
	if gotErr != nil {
		t.Fatalf("err = %v, want nil", gotErr)
	}
	want := []string{"response.created", "response.output_text.delta"}
	if !slices.Equal(types, want) {
		t.Errorf("types = %v, want %v (primary preamble dropped)", types, want)
	}
	if backup.calls != 1 {
		t.Errorf("backup.calls = %d, want 1", backup.calls)
	}
}

func TestFallbackModel_PrimaryFailsBackupSucceeds(t *testing.T) {
	primary := &scriptedModel{steps: []scriptStep{{nil, errBoom}}}
	backup := &scriptedModel{steps: []scriptStep{{&ModelResponse{ResponseID: "b"}, nil}}}
	m := NewFallbackModel(primary, backup)
	resp, err := m.GetResponse(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.ResponseID != "b" {
		t.Errorf("ResponseID = %q, want b", resp.ResponseID)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Errorf("calls primary=%d backup=%d", primary.calls, backup.calls)
	}
}

func TestFallbackModel_PrimarySucceedsBackupUntouched(t *testing.T) {
	primary := &scriptedModel{steps: []scriptStep{{&ModelResponse{ResponseID: "a"}, nil}}}
	backup := &scriptedModel{}
	m := NewFallbackModel(primary, backup)
	if _, err := m.GetResponse(context.Background(), ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if backup.calls != 0 {
		t.Errorf("backup.calls = %d, want 0", backup.calls)
	}
}

func TestFallbackModel_AllFailJoinsErrors(t *testing.T) {
	errA := errors.New("a-down")
	errB := errors.New("b-down")
	m := NewFallbackModel(
		&scriptedModel{steps: []scriptStep{{nil, errA}}},
		&scriptedModel{steps: []scriptStep{{nil, errB}}},
	)
	_, err := m.GetResponse(context.Background(), ModelRequest{})
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("err = %v, want both joined", err)
	}
}

func TestFallbackModel_ContextCancelStopsChain(t *testing.T) {
	primary := &scriptedModel{steps: []scriptStep{{nil, context.Canceled}}}
	backup := &scriptedModel{}
	m := NewFallbackModel(primary, backup)
	_, err := m.GetResponse(context.Background(), ModelRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if backup.calls != 0 {
		t.Errorf("backup.calls = %d, want 0 (no fallback on cancel)", backup.calls)
	}
}

func TestFallbackModel_StreamFallsBackBeforeFirstEvent(t *testing.T) {
	primary := &scriptedStreamModel{steps: []streamStep{{0, errBoom}}}
	backup := &scriptedStreamModel{steps: []streamStep{{2, nil}}}
	m := NewFallbackModel(primary, backup)
	events, err := drain(m.StreamResponse(context.Background(), ModelRequest{}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if events != 2 {
		t.Errorf("events = %d, want 2", events)
	}
}

func TestFallbackModel_StreamCommitsAfterFirstEvent(t *testing.T) {
	primary := &scriptedStreamModel{steps: []streamStep{{1, errBoom}}}
	backup := &scriptedStreamModel{steps: []streamStep{{3, nil}}}
	m := NewFallbackModel(primary, backup)
	events, err := drain(m.StreamResponse(context.Background(), ModelRequest{}))
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if events != 1 {
		t.Errorf("events = %d, want 1", events)
	}
	if backup.calls != 0 {
		t.Errorf("backup.calls = %d, want 0", backup.calls)
	}
}

type stubProvider struct {
	gotModel string
	model    Model
}

func (p *stubProvider) GetModel(name string) (Model, error) {
	p.gotModel = name
	return p.model, nil
}

func TestRouterProvider_RoutesByPrefix(t *testing.T) {
	groq := &stubProvider{model: &scriptedModel{}}
	oa := &stubProvider{model: &scriptedModel{}}
	r := NewRouterProvider(map[string]ModelProvider{"groq": groq, "openai": oa})

	if _, err := r.GetModel("groq/llama-3.3-70b"); err != nil {
		t.Fatal(err)
	}
	if groq.gotModel != "llama-3.3-70b" {
		t.Errorf("groq got %q", groq.gotModel)
	}
	if _, err := r.GetModel("openai/gpt-4o"); err != nil {
		t.Fatal(err)
	}
	if oa.gotModel != "gpt-4o" {
		t.Errorf("openai got %q", oa.gotModel)
	}
}

func TestRouterProvider_FallbackForUnprefixed(t *testing.T) {
	def := &stubProvider{model: &scriptedModel{}}
	r := NewRouterProvider(map[string]ModelProvider{"groq": &stubProvider{}}).WithFallback(def)
	if _, err := r.GetModel("gpt-4o"); err != nil {
		t.Fatal(err)
	}
	if def.gotModel != "gpt-4o" {
		t.Errorf("fallback got %q, want full name", def.gotModel)
	}
}

func TestRouterProvider_NoMatchNoFallbackErrors(t *testing.T) {
	r := NewRouterProvider(map[string]ModelProvider{"groq": &stubProvider{}})
	if _, err := r.GetModel("unknown/model"); err == nil {
		t.Fatal("expected error for unmatched prefix without fallback")
	}
}

func TestFallbackProvider_AllFallbacksFailReturnsAggregatedError(t *testing.T) {
	primary := &stubProvider{model: &scriptedModel{}}
	fp := NewFallbackProvider(primary,
		errModelProvider{err: errors.New("f1 down")},
		errModelProvider{err: errors.New("f2 down")},
	)
	m, err := fp.GetModel("x")
	if err == nil {
		t.Fatal("expected aggregated error when every fallback fails to resolve")
	}
	if m != nil {
		t.Errorf("model should be nil on total fallback failure, got %T", m)
	}
	if !strings.Contains(err.Error(), "f1 down") || !strings.Contains(err.Error(), "f2 down") {
		t.Errorf("error should aggregate both fallback failures: %v", err)
	}
}

func TestFallbackProvider_PartialFallbackKeepsChain(t *testing.T) {
	primary := &stubProvider{model: &scriptedModel{}}
	good := &stubProvider{model: &scriptedModel{}}
	fp := NewFallbackProvider(primary, errModelProvider{err: errors.New("bad down")}, good)
	m, err := fp.GetModel("x")
	if err != nil {
		t.Fatalf("partial resolution should not error: %v", err)
	}
	if _, ok := m.(*FallbackModel); !ok {
		t.Errorf("expected a *FallbackModel chaining the working fallback, got %T", m)
	}
}

func TestFallbackProvider_NoFallbacksReturnsPrimary(t *testing.T) {
	primary := &stubProvider{model: &scriptedModel{}}
	m, err := NewFallbackProvider(primary).GetModel("x")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*FallbackModel); ok {
		t.Errorf("with no fallbacks configured, expected the bare primary model, got *FallbackModel")
	}
}

func TestFallbackProvider_PrimaryFailurePropagates(t *testing.T) {
	fp := NewFallbackProvider(errModelProvider{err: errBoom}, &stubProvider{model: &scriptedModel{}})
	if _, err := fp.GetModel("x"); !errors.Is(err, errBoom) {
		t.Fatalf("primary failure should propagate, got %v", err)
	}
}
