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

// Both span constructors copy the caller's data, so whether a map may still be
// touched afterwards does not depend on which one was used.
func TestStartTypedSpan_CopiesCallerData(t *testing.T) {
	tr := NewTracer(&captureProc{}).StartTrace("wf")
	defer tr.Finish()

	data := map[string]any{"server": "fs"}
	sp := tr.StartSpan("mcp", "").StartTypedSpan("list_tools", "mcp_tools", data)
	data["server"] = "changed"

	if got := sp.Span.Data["server"]; got != "fs" {
		t.Errorf("Data[\"server\"] = %v, want the value at start", got)
	}
}

// A finished span belongs to the processor, whose exporter goroutine reads its
// Data map; a late Set is dropped rather than raced.
func TestSpanHandle_IgnoresWritesAfterFinish(t *testing.T) {
	tr := NewTracer(&captureProc{}).StartTrace("wf")
	defer tr.Finish()

	sp := tr.StartGenerationSpan("gpt-4o", "")
	sp.Set("response_id", "resp_1")
	sp.Finish()

	sp.Set("response_id", "resp_2")
	sp.SetError("too late", nil)

	if got := sp.Span.Data["response_id"]; got != "resp_1" {
		t.Errorf("Data[\"response_id\"] = %v, want the value set before Finish", got)
	}
	if sp.Span.Error != nil {
		t.Errorf("Error = %+v, want none: it was recorded after Finish", sp.Span.Error)
	}
}
