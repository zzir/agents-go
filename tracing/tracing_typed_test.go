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

// exportingProc mimics a background exporter: it keeps reading the span's data
// after OnSpanEnd returns, which is what BatchProcessor's goroutine does.
type exportingProc struct {
	got  chan *Span
	stop chan struct{}
	done chan struct{}
}

func (p *exportingProc) OnTraceStart(*Trace) {}
func (p *exportingProc) OnTraceEnd(*Trace)   {}
func (p *exportingProc) OnSpanStart(*Span)   {}
func (p *exportingProc) OnSpanEnd(s *Span) {
	p.got <- s
	go func() {
		defer close(p.done)
		for {
			select {
			case <-p.stop:
				return
			default:
			}
			for k, v := range s.Data {
				_, _ = k, v
			}
			_ = s.Error
		}
	}()
}
func (p *exportingProc) ForceFlush()              {}
func (p *exportingProc) Shutdown(context.Context) {}

// The finished flag is read before the write it guards, so an annotation can
// pass the check and land after the span has been handed over. Data is a map,
// where that is a process-killing fatal rather than a stale value — so the
// handover takes the same lock the annotations do. Under -race this test is
// the assertion.
func TestSpanHandle_AnnotatingRacesFinish(t *testing.T) {
	proc := &exportingProc{got: make(chan *Span, 1), stop: make(chan struct{}), done: make(chan struct{})}
	tr := NewTracer(proc).StartTrace("wf")
	defer tr.Finish()
	sp := tr.StartAgentSpan("a", "")
	sp.Set("before", 1)

	writing := make(chan struct{})
	go func() {
		close(writing)
		for i := range 2000 {
			sp.Set("k", i)
			sp.SetError("boom", nil)
		}
	}()
	<-writing
	sp.Finish()

	exported := <-proc.got
	if exported.Data["before"] != 1 {
		t.Fatalf("the export lost data set before Finish: %v", exported.Data)
	}
	if exported.EndedAt.IsZero() {
		t.Fatal("the exported span was not stamped")
	}
	close(proc.stop)
	<-proc.done
}
