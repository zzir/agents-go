package agents

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"
)

// failingModel always fails, counting how often it was asked.
type failingModel struct {
	err   error
	calls int
}

func (m *failingModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	m.calls++
	return nil, m.err
}

func (m *failingModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		m.calls++
		yield(nil, m.err)
	}
}

// A server hint longer than MaxDelay ends the retries instead of being clamped:
// retrying far sooner than the server said it would accept just burns the
// remaining attempts and reports the same failure later.
func TestRetryStopsOnOversizedServerHint(t *testing.T) {
	apiErr := errors.New("429 rate limited")
	var attempts, slept int
	model := &failingModel{err: apiErr}

	policy := RetryPolicy{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		MaxDelay:    30 * time.Second,
		RetryIf:     func(error) bool { attempts++; return true },
		RetryAfter:  func(error) (time.Duration, bool) { return 3 * time.Hour, true },
		sleep:       func(context.Context, time.Duration) error { slept++; return nil },
	}
	_, err := NewRetryModel(model, policy).GetResponse(context.Background(), ModelRequest{})

	if err == nil {
		t.Fatal("want an error")
	}
	if slept != 0 {
		t.Errorf("slept %d times; an over-cap hint must not sleep at all", slept)
	}
	if !errors.Is(err, apiErr) {
		t.Error("the original error was replaced rather than wrapped")
	}
	if !strings.Contains(err.Error(), "beyond the 30s limit") {
		t.Errorf("error does not say why it gave up: %v", err)
	}
	if model.calls != 1 {
		t.Errorf("model called %d times; should have stopped after the first hint", model.calls)
	}
}

// A hint within the cap is honored as the delay.
func TestRetryHonorsServerHintWithinCap(t *testing.T) {
	var delays []time.Duration
	model := &failingModel{err: errors.New("boom")}
	policy := RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   time.Hour, // backoff would be huge; the hint must win
		MaxDelay:    30 * time.Second,
		RetryIf:     func(error) bool { return true },
		RetryAfter:  func(error) (time.Duration, bool) { return 2 * time.Second, true },
		sleep:       func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil },
	}
	if _, err := NewRetryModel(model, policy).GetResponse(context.Background(), ModelRequest{}); err == nil {
		t.Fatal("want an error")
	}
	if len(delays) != 2 {
		t.Fatalf("slept %d times, want 2 (3 attempts)", len(delays))
	}
	for i, d := range delays {
		if d != 2*time.Second {
			t.Errorf("delay %d = %v, want the server hint 2s", i, d)
		}
	}
}
