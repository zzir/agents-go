package agents_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
)

// clockedModel fails its first `failures` Respond calls with `err`; a nil err
// blocks until the attempt's context ends and returns its error. Later calls
// succeed.
type clockedModel struct {
	failures int
	err      error
	calls    int
}

func (m *clockedModel) Respond(ctx context.Context, _ agents.ModelRequest) (*agents.ModelResponse, error) {
	m.calls++
	if m.calls > m.failures {
		return &agents.ModelResponse{}, nil
	}
	if m.err != nil {
		return nil, fmt.Errorf("attempt %d: %w", m.calls, m.err)
	}
	<-ctx.Done()
	return nil, fmt.Errorf("attempt %d: %w", m.calls, ctx.Err())
}

func (m *clockedModel) StreamResponse(context.Context, agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(func(*agents.ResponseStreamEvent, error) bool) {}
}

func quickPolicy(attempts int) agents.RetryPolicy {
	return agents.RetryPolicy{MaxAttempts: attempts, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
}

func TestRetryModel_AttemptTimeoutRetriesAndNamesItself(t *testing.T) {
	policy := quickPolicy(2)
	policy.AttemptTimeout = 30 * time.Millisecond

	hung := &clockedModel{failures: 1}
	if _, err := agents.NewRetryModel(hung, policy).Respond(context.Background(), agents.ModelRequest{}); err != nil {
		t.Fatalf("a hung first attempt was not replaced: %v", err)
	}
	if hung.calls != 2 {
		t.Fatalf("calls = %d, want 2", hung.calls)
	}

	always := &clockedModel{failures: 9}
	_, err := agents.NewRetryModel(always, policy).Respond(context.Background(), agents.ModelRequest{})
	if !errors.Is(err, agents.ErrAttemptTimeout) {
		t.Fatalf("error = %v, want ErrAttemptTimeout", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a policy timeout reads as the caller's own stop: %v", err)
	}
	if always.calls != 2 {
		t.Fatalf("calls = %d, want 2", always.calls)
	}
}

// The caller's cancellation is not the policy's clock: no retry, and the
// error is the caller's.
func TestRetryModel_CallerCancelIsNotAnAttemptTimeout(t *testing.T) {
	policy := quickPolicy(3)
	policy.AttemptTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	m := &clockedModel{failures: 9}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := agents.NewRetryModel(m, policy).Respond(ctx, agents.ModelRequest{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, agents.ErrAttemptTimeout) {
		t.Fatalf("error = %v, want the caller's cancellation", err)
	}
	if m.calls != 1 {
		t.Fatalf("calls = %d, want 1", m.calls)
	}
}

// A request that chains server-side state is retried only when the failure
// is known not to have applied it.
func TestRetryModel_StatefulRequestFailsClosedOnAnAmbiguousFailure(t *testing.T) {
	stateful := agents.ModelRequest{PreviousResponseID: "resp_1"}
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	cases := []struct {
		name  string
		req   agents.ModelRequest
		err   error
		calls int
	}{
		{"plain request, severed connection", agents.ModelRequest{}, io.ErrUnexpectedEOF, 2},
		{"stateful, severed connection", stateful, io.ErrUnexpectedEOF, 1},
		{"stateful, read timeout", stateful, &net.OpError{Op: "read", Err: context.DeadlineExceeded}, 1},
		{"stateful, dial failed", stateful, dial, 2},
		{"stateful, server answered", stateful, errors.New("429 too many requests"), 2},
		{"conversation, severed connection", agents.ModelRequest{ConversationID: "conv_1"}, io.EOF, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &clockedModel{failures: 1, err: tc.err}
			_, err := agents.NewRetryModel(m, quickPolicy(2)).Respond(context.Background(), tc.req)
			if (err == nil) != (tc.calls == 2) {
				t.Fatalf("err = %v with %d calls", err, m.calls)
			}
			if m.calls != tc.calls {
				t.Fatalf("calls = %d, want %d", m.calls, tc.calls)
			}
		})
	}

	// The policy's own timeout is ambiguous too: the request may have landed.
	policy := quickPolicy(2)
	policy.AttemptTimeout = 20 * time.Millisecond
	m := &clockedModel{failures: 1}
	_, err := agents.NewRetryModel(m, policy).Respond(context.Background(), stateful)
	if !errors.Is(err, agents.ErrAttemptTimeout) || m.calls != 1 {
		t.Fatalf("stateful timeout: err = %v, calls = %d; want ErrAttemptTimeout after one call", err, m.calls)
	}
}

// pacedStreamModel streams per the script: for each attempt, `before` output
// events are emitted (none for a stall), then the stream either stalls until
// its context ends or emits the rest `gap` apart and finishes.
type pacedStreamModel struct {
	script []pacedAttempt
	calls  int
}

type pacedAttempt struct {
	events int
	gap    time.Duration
	stall  bool // block after the events until the context ends
}

func (m *pacedStreamModel) Respond(context.Context, agents.ModelRequest) (*agents.ModelResponse, error) {
	return &agents.ModelResponse{}, nil
}

func (m *pacedStreamModel) StreamResponse(ctx context.Context, _ agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	i := m.calls
	m.calls++
	var a pacedAttempt
	if i < len(m.script) {
		a = m.script[i]
	}
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		for n := range a.events {
			if n > 0 && a.gap > 0 {
				select {
				case <-time.After(a.gap):
				case <-ctx.Done():
					yield(nil, ctx.Err())
					return
				}
			}
			if !yield(&agents.ResponseStreamEvent{Type: "response.output_text.delta"}, nil) {
				return
			}
		}
		if a.stall {
			<-ctx.Done()
			yield(nil, ctx.Err())
		}
	}
}

func collectStream(seq iter.Seq2[*agents.ResponseStreamEvent, error]) (events int, err error) {
	for ev, e := range seq {
		if e != nil {
			return events, e
		}
		if ev != nil {
			events++
		}
	}
	return events, nil
}

func TestRetryModel_IdleTimeoutRetriesBeforeOutputAndEndsAfter(t *testing.T) {
	policy := quickPolicy(2)
	policy.IdleTimeout = 30 * time.Millisecond

	// Silent before any output: the attempt is replaced.
	m := &pacedStreamModel{script: []pacedAttempt{{stall: true}, {events: 2}}}
	events, err := collectStream(agents.NewRetryModel(m, policy).StreamResponse(context.Background(), agents.ModelRequest{}))
	if err != nil || events != 2 || m.calls != 2 {
		t.Fatalf("pre-output stall: events = %d, err = %v, calls = %d; want 2, nil, 2", events, err, m.calls)
	}

	// Silent after output: committed, so the stream ends with the idle error.
	m = &pacedStreamModel{script: []pacedAttempt{{events: 1, stall: true}}}
	events, err = collectStream(agents.NewRetryModel(m, policy).StreamResponse(context.Background(), agents.ModelRequest{}))
	if !errors.Is(err, agents.ErrIdleTimeout) || events != 1 || m.calls != 1 {
		t.Fatalf("post-output stall: events = %d, err = %v, calls = %d; want 1, ErrIdleTimeout, 1", events, err, m.calls)
	}
}

// The attempt clock covers a stream only until it commits: a long answer
// that started promptly is not cut.
func TestRetryModel_AttemptClockReleasesAtCommit(t *testing.T) {
	policy := quickPolicy(1)
	policy.AttemptTimeout = 200 * time.Millisecond
	m := &pacedStreamModel{script: []pacedAttempt{{events: 6, gap: 60 * time.Millisecond}}}
	events, err := collectStream(agents.NewRetryModel(m, policy).StreamResponse(context.Background(), agents.ModelRequest{}))
	if err != nil || events != 6 {
		t.Fatalf("events = %d, err = %v; want 6, nil", events, err)
	}
}

func TestRetryPolicy_JSONCarriesTheTimeouts(t *testing.T) {
	in := agents.RetryPolicy{MaxAttempts: 2, AttemptTimeout: 1500 * time.Millisecond, IdleTimeout: 20 * time.Second}
	raw, err := in.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out agents.RetryPolicy
	if err := out.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	if out.AttemptTimeout != in.AttemptTimeout || out.IdleTimeout != in.IdleTimeout {
		t.Fatalf("round trip lost the timeouts: %s -> %+v", raw, out)
	}
	var none agents.RetryPolicy
	if raw, _ := none.MarshalJSON(); string(raw) != `{"max_attempts":0,"base_delay_ms":0,"max_delay_ms":0,"multiplier":0}` {
		t.Fatalf("a zero policy grew new keys: %s", raw)
	}
}
