package agents

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/tracing"
)

// spanCapture invokes onEnd for each finished span (synchronous, no flush needed).
type spanCapture struct{ onEnd func(*tracing.Span) }

func (spanCapture) OnTraceStart(*tracing.Trace) {}
func (spanCapture) OnTraceEnd(*tracing.Trace)   {}
func (spanCapture) OnSpanStart(*tracing.Span)   {}
func (c spanCapture) OnSpanEnd(s *tracing.Span) { c.onEnd(s) }
func (spanCapture) ForceFlush()                 {}
func (spanCapture) Shutdown(context.Context)    {}

func TestGenerationSpanRecordsUsage(t *testing.T) {
	var gen *tracing.Span
	proc := spanCapture{onEnd: func(s *tracing.Span) {
		if s.Type == tracing.SpanTypeGeneration {
			gen = s
		}
	}}

	agent := &Agent{Name: "a", Instructions: StaticInstructions("x")}
	model := &fakeModel{responses: []*ModelResponse{
		{ResponseID: "r1", Usage: &Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
	}}

	_, err := RunSync(context.Background(), agent, "hi", RunOptions{Model: ModelOptions{Override: model}, Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}})
	if err != nil {
		t.Fatal(err)
	}
	if gen == nil {
		t.Fatal("no generation span was recorded")
	}
	if gen.Data["input_tokens"] != int64(10) {
		t.Errorf("input_tokens = %v, want 10", gen.Data["input_tokens"])
	}
	if gen.Data["output_tokens"] != int64(5) {
		t.Errorf("output_tokens = %v, want 5", gen.Data["output_tokens"])
	}
	if gen.Data["total_tokens"] != int64(15) {
		t.Errorf("total_tokens = %v, want 15", gen.Data["total_tokens"])
	}
	if gen.Data["response_id"] != "r1" {
		t.Errorf("response_id = %v, want r1", gen.Data["response_id"])
	}
}
