package agents

import (
	"context"
	"errors"
	"iter"
	"testing"
)

// cancelStreamModel cancels the run's context as its stream starts, then yields a
// context.Canceled error — reproducing a provider surfacing a mid-stream
// cancellation (e.g. "openai responses stream: context canceled").
type cancelStreamModel struct{ cancel context.CancelFunc }

func (m *cancelStreamModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return nil, context.Canceled
}

func (m *cancelStreamModel) StreamResponse(ctx context.Context, _ ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		m.cancel()
		yield(nil, context.Canceled)
	}
}

// A streamed run cancelled mid-stream must report its terminal error via
// FinalResult even when the Events channel drops it (RunStreamed's error send
// loses the select to ctx.Done()). agents-server relies on this: it consults
// FinalResult after draining Events so an aborted run never vanishes.
func TestRunStreamed_TerminalErrorAlwaysInFinalResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	agent := &Agent{Name: "a", ModelImpl: &cancelStreamModel{cancel: cancel}}

	sr := RunStreamed(ctx, agent, "hi", RunOptions{})

	// Drain the events WITHOUT inspecting the per-item error, mimicking the drop:
	// the failure may or may not be delivered here.
	for range sr.Events() { //nolint:revive // intentional drain
	}

	_, err := sr.FinalResult()
	if err == nil {
		t.Fatal("FinalResult must surface the terminal error even when Events drops it")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("FinalResult err = %v, want context.Canceled", err)
	}
}
