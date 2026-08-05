package agents

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/tracing"
)

// A consumer that abandons the stream must still leave the trace closed. This
// was the open question the pull model had to answer: with a producer goroutine
// there was a real gap between "the consumer stopped reading" and "the run
// noticed", and a trace could stay open forever.
//
// It closes because the run executes inside the iterator: yield returning false
// unwinds the loop, runStream returns, and its deferred finishTrace runs. There
// is no window in which nobody owns the trace.
func TestTrace_ClosedWhenConsumerAbandonsStream(t *testing.T) {
	loopTool := NewTool("loop", "loops",
		func(context.Context, *ToolContext, struct{}) (string, error) { return "again", nil })
	newAgent := func() *Agent {
		return &Agent{Name: "a", Tools: []*Tool{loopTool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
			modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
			modelResp(messageOutput(t, "done")),
		}}}
	}

	collect := func(stop func(int) bool) []tracing.Item {
		exporter := &tracing.CollectingExporter{}
		proc := tracing.NewBatchProcessor(exporter, tracing.BatchProcessorOptions{})
		opts := RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}

		stream, _ := Run(context.Background(), newAgent(), "go", opts)
		seen := 0
		for range stream {
			seen++
			if stop(seen) {
				break
			}
		}
		proc.ForceFlush()
		return exporter.Items()
	}

	full := collect(func(int) bool { return false })
	abandoned := collect(func(n int) bool { return n == 2 }) // bail after two events

	spanTypes := func(items []tracing.Item) map[string]int {
		out := map[string]int{}
		for _, it := range items {
			if s, ok := it.(*tracing.Span); ok {
				out[s.Type]++
			}
		}
		return out
	}

	// The completed run records its agent span.
	if spanTypes(full)[tracing.SpanTypeAgent] == 0 {
		t.Fatal("a completed run recorded no agent span")
	}

	// So does the abandoned one — every span it opened was finished and
	// exported, rather than left dangling.
	if got := spanTypes(abandoned)[tracing.SpanTypeAgent]; got == 0 {
		t.Errorf("abandoning the stream left the agent span unfinished (exported %d agent spans)", got)
	}
	for _, it := range abandoned {
		s, ok := it.(*tracing.Span)
		if !ok {
			continue
		}
		if s.EndedAt.IsZero() {
			t.Errorf("span %q (%s) was exported without an end time", s.Name, s.Type)
		}
	}

	// And it really did stop early, or the test proves nothing.
	if len(abandoned) >= len(full) {
		t.Errorf("abandoned run exported %d items, full run %d — it did not stop early",
			len(abandoned), len(full))
	}
}
