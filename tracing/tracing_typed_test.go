package tracing

import (
	"context"
	"testing"
)

// captureProc records spans as they start.
type captureProc struct{ spans []*Span }

func (p *captureProc) OnTraceStart(*Trace)      {}
func (p *captureProc) OnTraceEnd(*Trace)        {}
func (p *captureProc) OnSpanStart(s *Span)      { p.spans = append(p.spans, s) }
func (p *captureProc) OnSpanEnd(*Span)          {}
func (p *captureProc) ForceFlush()              {}
func (p *captureProc) Shutdown(context.Context) {}

func TestTypedSpanConstructors(t *testing.T) {
	tr := NewTracer(&captureProc{}).StartTrace("wf")
	defer tr.Finish()

	cases := []struct {
		sp                 *SpanHandle
		wantType, wantName string
		key, keyVal        string
	}{
		{tr.StartAgentSpan("triage", ""), SpanTypeAgent, "agent:triage", "name", "triage"},
		{tr.StartGenerationSpan("gpt-4o", ""), SpanTypeGeneration, "generation:gpt-4o", "name", "gpt-4o"},
		{tr.StartFunctionSpan("get_weather", ""), SpanTypeFunction, "function:get_weather", "name", "get_weather"},
		{tr.StartHandoffSpan("transfer_to_billing", ""), SpanTypeHandoff, "handoff:transfer_to_billing", "name", "transfer_to_billing"},
		{tr.StartGuardrailSpan("input", ""), SpanTypeGuardrail, "guardrail:input", "stage", "input"},
	}
	for _, c := range cases {
		if c.sp.Span.Type != c.wantType {
			t.Errorf("Type = %q, want %q", c.sp.Span.Type, c.wantType)
		}
		// Name keeps the legacy prefix form for back-compat.
		if c.sp.Span.Name != c.wantName {
			t.Errorf("Name = %q, want %q", c.sp.Span.Name, c.wantName)
		}
		if got := c.sp.Span.Data[c.key]; got != c.keyVal {
			t.Errorf("%s span Data[%q] = %v, want %q", c.wantType, c.key, got, c.keyVal)
		}
	}

	// The untyped StartSpan leaves Type empty.
	if u := tr.StartSpan("custom", ""); u.Span.Type != "" {
		t.Errorf("untyped span Type = %q, want empty", u.Span.Type)
	}
}
