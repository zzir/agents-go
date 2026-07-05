package agents

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zzir/agents-go/tracing"
)

// recordingProcessor captures finished spans for assertions.
type recordingProcessor struct {
	mu    sync.Mutex
	spans []*tracing.Span
}

func (p *recordingProcessor) OnTraceStart(*tracing.Trace) {}
func (p *recordingProcessor) OnTraceEnd(*tracing.Trace)   {}
func (p *recordingProcessor) OnSpanStart(*tracing.Span)   {}
func (p *recordingProcessor) OnSpanEnd(s *tracing.Span) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spans = append(p.spans, s)
}
func (p *recordingProcessor) ForceFlush()              {}
func (p *recordingProcessor) Shutdown(context.Context) {}

func (p *recordingProcessor) generationSpans() []*tracing.Span {
	return p.spansOfType(tracing.SpanTypeGeneration)
}

func (p *recordingProcessor) spansOfType(typ string) []*tracing.Span {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*tracing.Span
	for _, s := range p.spans {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

// compactingSession wraps InMemorySession with a RunCompaction that follows
// the CompactionArgs.StartSpan contract: no-op below the threshold, otherwise
// open the span and annotate it.
type compactingSession struct {
	*InMemorySession
	threshold int
	fail      bool
}

func (s *compactingSession) RunCompaction(ctx context.Context, args CompactionArgs) error {
	items, err := s.GetItems(ctx, 0)
	if err != nil {
		return err
	}
	if len(items) < s.threshold {
		return nil
	}
	var span *tracing.SpanHandle
	if args.StartSpan != nil {
		span = args.StartSpan()
	}
	span.Set("before_items", len(items))
	span.Set("after_items", 1)
	if s.fail {
		return errors.New("summarize exploded")
	}
	return nil
}

// Function spans carry the tool call's arguments and stringified result
// (Python parity: FunctionSpanData.input/output), gated by the same
// sensitive-data switch as generation payloads.
func TestFunctionSpanRecordsInputOutput(t *testing.T) {
	agent, proc := tracingAgent(t)
	agent.ModelImpl = &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "call_1", `{"city":"SF"}`)),
		modelResp(messageOutput(t, "done")),
	}}
	if _, err := Run(context.Background(), agent, "hi", RunOptions{Tracer: tracing.NewTracer(proc)}); err != nil {
		t.Fatal(err)
	}
	fns := proc.spansOfType(tracing.SpanTypeFunction)
	if len(fns) != 1 {
		t.Fatalf("want 1 function span, got %d", len(fns))
	}
	d := fns[0].Data
	if d["input"] != `{"city":"SF"}` || d["output"] != "sunny" {
		t.Fatalf("function span input/output not recorded: %v", d)
	}

	// Sensitive-data opt-out keeps arguments and results off the span.
	agent2, proc2 := tracingAgent(t)
	agent2.ModelImpl = &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "call_1", `{"city":"SF"}`)),
		modelResp(messageOutput(t, "done")),
	}}
	include := false
	if _, err := Run(context.Background(), agent2, "hi", RunOptions{
		Tracer:                    tracing.NewTracer(proc2),
		TraceIncludeSensitiveData: &include,
	}); err != nil {
		t.Fatal(err)
	}
	fns = proc2.spansOfType(tracing.SpanTypeFunction)
	if len(fns) != 1 {
		t.Fatalf("want 1 function span, got %d", len(fns))
	}
	for _, k := range []string{"input", "output"} {
		if _, ok := fns[0].Data[k]; ok {
			t.Fatalf("sensitive key %q recorded despite opt-out: %v", k, fns[0].Data)
		}
	}
}

// The runner wraps RunCompaction in a compaction span — but only when the
// session actually compacts; no-op passes must not emit a span. Errors from
// RunCompaction land on the span.
func TestCompactionSpan(t *testing.T) {
	t.Run("noop pass emits no span", func(t *testing.T) {
		agent, proc := tracingAgent(t)
		sess := &compactingSession{InMemorySession: NewInMemorySession(), threshold: 100}
		if _, err := Run(context.Background(), agent, "hi", RunOptions{Tracer: tracing.NewTracer(proc), Session: sess}); err != nil {
			t.Fatal(err)
		}
		if got := proc.spansOfType(tracing.SpanTypeCompaction); len(got) != 0 {
			t.Fatalf("no-op compaction emitted %d spans", len(got))
		}
	})

	t.Run("real pass emits annotated span", func(t *testing.T) {
		agent, proc := tracingAgent(t)
		sess := &compactingSession{InMemorySession: NewInMemorySession(), threshold: 1}
		if _, err := Run(context.Background(), agent, "hi", RunOptions{Tracer: tracing.NewTracer(proc), Session: sess}); err != nil {
			t.Fatal(err)
		}
		spans := proc.spansOfType(tracing.SpanTypeCompaction)
		if len(spans) != 1 {
			t.Fatalf("want 1 compaction span, got %d", len(spans))
		}
		if spans[0].Data["before_items"] == nil || spans[0].Data["after_items"] == nil {
			t.Fatalf("compaction span missing counts: %v", spans[0].Data)
		}
		if spans[0].Error != nil {
			t.Fatalf("unexpected span error: %v", spans[0].Error)
		}
	})

	t.Run("failure lands on span, run still succeeds", func(t *testing.T) {
		agent, proc := tracingAgent(t)
		sess := &compactingSession{InMemorySession: NewInMemorySession(), threshold: 1, fail: true}
		// Compaction is best-effort housekeeping after the run's items are
		// saved: its failure is recorded on the span, not returned to the
		// caller whose run already produced a final output.
		res, err := Run(context.Background(), agent, "hi", RunOptions{Tracer: tracing.NewTracer(proc), Session: sess})
		if err != nil {
			t.Fatalf("compaction failure must not fail the run: %v", err)
		}
		if res.FinalOutputString() == "" {
			t.Fatal("final output lost on compaction failure")
		}
		spans := proc.spansOfType(tracing.SpanTypeCompaction)
		if len(spans) != 1 || spans[0].Error == nil || spans[0].Error.Message != "summarize exploded" {
			t.Fatalf("compaction error not on span: %+v", spans)
		}
	})
}

func tracingAgent(t *testing.T) (*Agent, *recordingProcessor) {
	t.Helper()
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "final"))}}
	tool := NewFunctionTool("get_weather", "weather lookup",
		func(ctx context.Context, tc *ToolContext, args struct {
			City string `json:"city"`
		}) (string, error) {
			return "sunny", nil
		})
	temp := 0.5
	agent := &Agent{
		Name:          "a",
		Model:         "fake-model",
		Instructions:  StaticInstructions("be brief"),
		ModelImpl:     model,
		Tools:         []Tool{tool},
		ModelSettings: &ModelSettings{Temperature: &temp},
	}
	return agent, &recordingProcessor{}
}

// Generation spans carry the full model request (model, instructions, input)
// and the output items by default, so trace consumers can see exactly what
// each call sent and got back.
func TestGenerationSpanRecordsRequestAndOutput(t *testing.T) {
	agent, proc := tracingAgent(t)
	if _, err := Run(context.Background(), agent, "hi", RunOptions{Tracer: tracing.NewTracer(proc)}); err != nil {
		t.Fatal(err)
	}
	gens := proc.generationSpans()
	if len(gens) != 1 {
		t.Fatalf("want 1 generation span, got %d", len(gens))
	}
	d := gens[0].Data
	if d["model"] != "fake-model" || d["system_instructions"] != "be brief" {
		t.Fatalf("request meta missing: %v", d)
	}
	in, ok := d["input"].([]TResponseInputItem)
	if !ok || len(in) != 1 {
		t.Fatalf("input not recorded: %#v", d["input"])
	}
	out, ok := d["output"].([]TResponseOutputItem)
	if !ok || len(out) != 1 {
		t.Fatalf("output not recorded: %#v", d["output"])
	}
	tools, ok := d["tools"].([]map[string]any)
	if !ok || len(tools) != 1 || tools[0]["name"] != "get_weather" || tools[0]["parameters"] == nil {
		t.Fatalf("tool definitions not recorded: %#v", d["tools"])
	}
	settings, ok := d["model_settings"].(ModelSettings)
	if !ok || settings.Temperature == nil || *settings.Temperature != 0.5 {
		t.Fatalf("model settings not recorded: %#v", d["model_settings"])
	}
}

// The streaming runner records the same request/output data on its spans.
func TestGenerationSpanRecordsRequestAndOutputStreamed(t *testing.T) {
	agent, proc := tracingAgent(t)
	sr := RunStreamed(context.Background(), agent, "hi", RunOptions{Tracer: tracing.NewTracer(proc)})
	for _, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
	}
	gens := proc.generationSpans()
	if len(gens) != 1 {
		t.Fatalf("want 1 generation span, got %d", len(gens))
	}
	d := gens[0].Data
	if _, ok := d["input"].([]TResponseInputItem); !ok {
		t.Fatalf("streamed input not recorded: %#v", d["input"])
	}
	if _, ok := d["output"].([]TResponseOutputItem); !ok {
		t.Fatalf("streamed output not recorded: %#v", d["output"])
	}
	if _, ok := d["time_to_first_token_ms"].(int64); !ok {
		t.Fatalf("ttft not recorded: %#v", d["time_to_first_token_ms"])
	}
}

// TraceIncludeSensitiveData=false keeps conversation content out of spans
// while retaining ids and usage.
func TestGenerationSpanExcludesSensitiveData(t *testing.T) {
	agent, proc := tracingAgent(t)
	include := false
	if _, err := Run(context.Background(), agent, "hi", RunOptions{
		Tracer:                    tracing.NewTracer(proc),
		TraceIncludeSensitiveData: &include,
	}); err != nil {
		t.Fatal(err)
	}
	gens := proc.generationSpans()
	if len(gens) != 1 {
		t.Fatalf("want 1 generation span, got %d", len(gens))
	}
	d := gens[0].Data
	for _, k := range []string{"model", "system_instructions", "input", "output", "tools", "model_settings", "handoffs"} {
		if _, ok := d[k]; ok {
			t.Fatalf("sensitive key %q recorded despite opt-out: %v", k, d)
		}
	}
	if _, ok := d["total_tokens"]; !ok {
		t.Fatalf("usage should still be recorded: %v", d)
	}
}

// The OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA env var supplies the default
// when RunOptions leaves the flag nil; "false" opts out.
func TestGenerationSpanEnvOptOut(t *testing.T) {
	t.Setenv("OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA", "false")
	agent, proc := tracingAgent(t)
	if _, err := Run(context.Background(), agent, "hi", RunOptions{Tracer: tracing.NewTracer(proc)}); err != nil {
		t.Fatal(err)
	}
	if d := proc.generationSpans()[0].Data; d["input"] != nil {
		t.Fatalf("env opt-out ignored: %v", d)
	}
}
