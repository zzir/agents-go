package agents

import (
	"context"
	"testing"
	"time"

	"github.com/zzir/agents-go/tracing"
)

// A generation span that took eight seconds because it was tried three times is
// otherwise indistinguishable from one that was simply slow.
func TestSpans_RetriesNestUnderTheGenerationSpan(t *testing.T) {
	proc := &recordingProcessor{}
	inner := &flakyModel{failures: 2, answer: modelResp(messageOutput(t, "finally"))}
	agent := &Agent{Name: "a", ModelImpl: NewRetryModel(inner, RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond})}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{
		Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)},
	}); err != nil {
		t.Fatal(err)
	}

	retries := proc.spansOfType(tracing.SpanTypeModelRetry)
	if len(retries) != 2 {
		t.Fatalf("%d retry spans, want 2", len(retries))
	}
	gen := proc.spansOfType(tracing.SpanTypeGeneration)
	if len(gen) != 1 {
		t.Fatalf("%d generation spans, want 1", len(gen))
	}
	for _, r := range retries {
		if r.ParentID != gen[0].SpanID {
			t.Errorf("retry span parented to %q, want the generation span %q", r.ParentID, gen[0].SpanID)
		}
		if r.Error == nil {
			t.Error("a retry span records no reason")
		}
	}
}

// A subsystem used outside a run must behave exactly as it did before it was
// instrumented.
func TestSpans_NoTracerIsANoOp(t *testing.T) {
	sp, ctx := tracing.StartSpanFrom(context.Background(), "x", tracing.SpanTypeMCP, nil)
	if sp == nil {
		t.Fatal("StartSpanFrom returned nil without a trace")
	}
	// Must not panic.
	sp.Set("k", "v")
	sp.SetError("boom", nil)
	sp.Finish()
	if tracing.SpanFrom(ctx) != nil {
		t.Error("a no-op span was installed on the context")
	}
}

// Tool work nests under the function span, so an MCP round trip or a sandbox
// exec shows up where the call that caused it is.
func TestSpans_ToolWorkNestsUnderTheFunctionSpan(t *testing.T) {
	proc := &recordingProcessor{}
	var seen *tracing.SpanHandle
	tool := NewFunctionTool("probe", "", func(ctx context.Context, _ *ToolContext, _ struct{}) (string, error) {
		seen = tracing.SpanFrom(ctx)
		child, _ := tracing.StartSpanFrom(ctx, "sandbox.exec", tracing.SpanTypeSandbox, nil)
		child.Finish()
		return "ok", nil
	})
	agent := &Agent{Name: "a", Tools: []*FunctionTool{tool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{
		Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)},
	}); err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("the tool received no parent span")
	}
	sandboxSpans := proc.spansOfType(tracing.SpanTypeSandbox)
	if len(sandboxSpans) != 1 {
		t.Fatalf("%d sandbox spans, want 1", len(sandboxSpans))
	}
	parent := spanByID(proc, sandboxSpans[0].ParentID)
	if parent == nil || parent.Type != tracing.SpanTypeFunction {
		t.Errorf("sandbox span parented to %v, want the function span", parent)
	}
}

func TestSpans_TypedChildCarriesItsType(t *testing.T) {
	proc := &recordingProcessor{}
	tr := tracing.NewTracer(proc).StartTrace("w")
	parent := tr.StartAgentSpan("a", "")
	child := parent.StartTypedSpan("mcp.call_tool", tracing.SpanTypeMCP, map[string]any{"server": "s1"})
	child.Finish()
	parent.Finish()
	tr.Finish()

	got := proc.spansOfType(tracing.SpanTypeMCP)
	if len(got) != 1 {
		t.Fatalf("%d MCP spans, want 1", len(got))
	}
	if got[0].Data["server"] != "s1" {
		t.Errorf("data = %v, want the server name", got[0].Data)
	}
	if got[0].ParentID != parent.Span.SpanID {
		t.Error("typed child is not parented to its span")
	}
}

// spanByID finds a recorded span by id.
func spanByID(p *recordingProcessor, id string) *tracing.Span {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.spans {
		if s.SpanID == id {
			return s
		}
	}
	return nil
}
