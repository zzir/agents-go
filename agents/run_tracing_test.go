package agents

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/zzir/agents-go/agents/session"
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
// the session.CompactionArgs.StartSpan contract: no-op below the threshold, otherwise
// open the span and annotate it.
type compactingSession struct {
	*session.InMemoryStorage
	threshold int
	fail      bool
}

func (s *compactingSession) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	items, err := session.NewSession(s).ContextItems(ctx, session.Cursor{})
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

// Function spans carry the tool call's arguments and stringified result, gated
// by the same sensitive-data switch as generation payloads.
func TestFunctionSpanRecordsInputOutput(t *testing.T) {
	agent, proc := tracingAgent(t)
	agent.ModelImpl = &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "call_1", `{"city":"SF"}`)),
		modelResp(messageOutput(t, "done")),
	}}
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
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
	if _, err := RunSync(context.Background(), agent2, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc2), IncludeSensitiveData: &include}}); err != nil {
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
		sess := &compactingSession{InMemoryStorage: session.NewInMemoryStorage("test"), threshold: 100}
		if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}, Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
			t.Fatal(err)
		}
		if got := proc.spansOfType(tracing.SpanTypeCompaction); len(got) != 0 {
			t.Fatalf("no-op compaction emitted %d spans", len(got))
		}
	})

	t.Run("real pass emits annotated span", func(t *testing.T) {
		agent, proc := tracingAgent(t)
		sess := &compactingSession{InMemoryStorage: session.NewInMemoryStorage("test"), threshold: 1}
		if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}, Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
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
		sess := &compactingSession{InMemoryStorage: session.NewInMemoryStorage("test"), threshold: 1, fail: true}
		// Compaction is best-effort housekeeping after the run's items are
		// saved: its failure is recorded on the span, not returned to the
		// caller whose run already produced a final output.
		res, err := RunSync(context.Background(), agent, "hi", RunOptions{Conversation: ConversationOptions{Session: session.NewSession(sess)}, Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}})
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
	tool := NewTool("get_weather", "weather lookup",
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
		Tools:         []*Tool{tool},
		ModelSettings: &ModelSettings{Temperature: &temp},
	}
	return agent, &recordingProcessor{}
}

// Generation spans carry the full model request (model, instructions, input)
// and the output items by default, so trace consumers can see exactly what
// each call sent and got back.
func TestGenerationSpanRecordsRequestAndOutput(t *testing.T) {
	agent, proc := tracingAgent(t)
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
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
	in, ok := d["input"].([]InputItem)
	if !ok || len(in) != 1 {
		t.Fatalf("input not recorded: %#v", d["input"])
	}
	out, ok := d["output"].([]OutputItem)
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
	stream, _ := Run(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}})
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	gens := proc.generationSpans()
	if len(gens) != 1 {
		t.Fatalf("want 1 generation span, got %d", len(gens))
	}
	d := gens[0].Data
	if _, ok := d["input"].([]InputItem); !ok {
		t.Fatalf("streamed input not recorded: %#v", d["input"])
	}
	if _, ok := d["output"].([]OutputItem); !ok {
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
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc), IncludeSensitiveData: &include}}); err != nil {
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

// A nil IncludeSensitiveData defaults to include: no environment variable is
// consulted, so leaving the option unset records the full request (spec §2.14).
func TestGenerationSpanDefaultsInclude(t *testing.T) {
	agent, proc := tracingAgent(t)
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
		t.Fatal(err)
	}
	if d := proc.generationSpans()[0].Data; d["input"] == nil {
		t.Fatalf("nil IncludeSensitiveData should default to include: %v", d)
	}
}

// Tool errors routinely embed the call arguments, so the function span's error
// message is redacted when sensitive-data tracing is off.
func TestFunctionSpanErrorRedaction(t *testing.T) {
	newRun := func(include bool) *tracing.Span {
		t.Helper()
		tool := NewTool("boom", "fails",
			func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
				return "", errors.New("secret-arg-value leaked")
			})
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
			modelResp(messageOutput(t, "done")),
		}}
		agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}
		proc := &recordingProcessor{}
		if _, err := RunSync(context.Background(), agent, "go", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc), IncludeSensitiveData: &include}}); err != nil {
			t.Fatal(err)
		}
		fns := proc.spansOfType(tracing.SpanTypeFunction)
		if len(fns) != 1 {
			t.Fatalf("want 1 function span, got %d", len(fns))
		}
		return fns[0]
	}

	span := newRun(false)
	if span.Error == nil {
		t.Fatal("function span should record the error")
	}
	if span.Error.Message != "Tool execution failed. Error details are redacted." {
		t.Errorf("redacted message = %q", span.Error.Message)
	}

	span = newRun(true)
	if span.Error == nil || span.Error.Message != "secret-arg-value leaked" {
		t.Errorf("with sensitive data on, error = %+v, want the raw message", span.Error)
	}
}

// A generation span carries the provider's ids and verdict on the call — the
// request id a support ticket needs, the model that actually answered, the
// status — and a usage detail count only when the provider reported one.
func TestGenerationSpanRecordsResponseMeta(t *testing.T) {
	agent, proc := tracingAgent(t)
	resp := modelResp(messageOutput(t, "final"))
	resp.RequestID, resp.Model, resp.Status = "req_1", "fake-model-2026-01", "completed"
	resp.Usage = &Usage{Requests: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		InputTokensDetails:  InputTokensDetails{CachedTokens: 7, CacheWriteTokens: 2},
		OutputTokensDetails: OutputTokensDetails{ReasoningTokens: 3}}
	agent.ModelImpl = &fakeModel{responses: []*ModelResponse{resp}}
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
		t.Fatal(err)
	}
	d := proc.generationSpans()[0].Data
	want := map[string]any{
		"request_id": "req_1", "model_used": "fake-model-2026-01", "status": "completed",
		"cached_tokens": int64(7), "cache_write_tokens": int64(2), "reasoning_tokens": int64(3),
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("%s = %#v, want %#v", k, d[k], v)
		}
	}
	if _, ok := d["incomplete_reason"]; ok {
		t.Errorf("a completed response carries no incomplete_reason: %v", d)
	}

	// Nothing reported, nothing recorded: most calls have no cache figure,
	// and an absent key is what the panel reads as "none".
	agent2, proc2 := tracingAgent(t)
	if _, err := RunSync(context.Background(), agent2, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc2)}}); err != nil {
		t.Fatal(err)
	}
	d = proc2.generationSpans()[0].Data
	for _, k := range []string{"cached_tokens", "cache_write_tokens", "reasoning_tokens", "request_id", "model_used", "status"} {
		if _, ok := d[k]; ok {
			t.Errorf("%s recorded without the provider reporting it: %v", k, d)
		}
	}
}

// A streamed call that dies mid-message leaves what it had produced on its
// span — the items that completed, the text in flight — under the same gate
// as output.
func TestGenerationSpanRecordsPartialOutputOnStreamFailure(t *testing.T) {
	const doneItem = `{"type":"message","id":"m0","role":"assistant","status":"completed","content":[{"type":"output_text","text":"first","annotations":[]}]}`
	run := func(include bool) *tracing.Span {
		t.Helper()
		model := &eventStreamModel{
			events: []ResponseStreamEvent{
				mustStreamEvent(t, `{"type":"response.output_item.done","output_index":0,"sequence_number":1,"item":`+doneItem+`}`),
				mustStreamEvent(t, `{"type":"response.output_text.delta","item_id":"m1","output_index":1,"content_index":0,"delta":"hel","sequence_number":2}`),
				mustStreamEvent(t, `{"type":"response.output_text.delta","item_id":"m1","output_index":1,"content_index":0,"delta":"lo","sequence_number":3}`),
			},
			err: errors.New("connection reset"),
		}
		agent := &Agent{Name: "a", ModelImpl: model}
		proc := &recordingProcessor{}
		stream, _ := Run(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc), IncludeSensitiveData: &include}})
		var runErr error
		for _, err := range stream {
			if err != nil {
				runErr = err
			}
		}
		if runErr == nil {
			t.Fatal("a stream that failed must fail the run")
		}
		gens := proc.generationSpans()
		if len(gens) != 1 || gens[0].Error == nil {
			t.Fatalf("want 1 failed generation span, got %+v", gens)
		}
		return gens[0]
	}

	span := run(true)
	if span.Data["partial_text"] != "hello" {
		t.Errorf("partial_text = %#v, want the streamed text", span.Data["partial_text"])
	}
	if out, ok := span.Data["output"].([]OutputItem); !ok || len(out) != 1 {
		t.Errorf("output = %#v, want the one completed item", span.Data["output"])
	}

	span = run(false)
	for _, k := range []string{"partial_text", "output"} {
		if _, ok := span.Data[k]; ok {
			t.Errorf("%s recorded despite the sensitive-data opt-out: %v", k, span.Data)
		}
	}
}

// An agent span says how its tenure ended — a final output, a handoff, a pause
// for approval — so a trace tells a paused run from a finished one.
func TestAgentSpanRecordsHowItEnded(t *testing.T) {
	agentSpans := func(proc *recordingProcessor) map[string]*tracing.Span {
		out := map[string]*tracing.Span{}
		for _, s := range proc.spansOfType(tracing.SpanTypeAgent) {
			out[s.Name] = s
		}
		return out
	}

	t.Run("final output", func(t *testing.T) {
		agent, proc := tracingAgent(t)
		if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
			t.Fatal(err)
		}
		d := agentSpans(proc)["agent:a"].Data
		if d["ended_by"] != "final_output" {
			t.Errorf("ended_by = %#v, want final_output", d["ended_by"])
		}
		if _, ok := d["stopped_early"]; ok {
			t.Errorf("stopped_early recorded on a run nobody stopped: %v", d)
		}
	})

	t.Run("interruption names the pending tools", func(t *testing.T) {
		agent, proc := tracingAgent(t)
		agent.Tools[0].NeedsApproval = true
		agent.ModelImpl = &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "get_weather", "c1", `{"city":"SF"}`)),
		}}
		res, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}})
		if err != nil || len(res.Interruptions) != 1 {
			t.Fatalf("want a paused run, got %+v / %v", res, err)
		}
		d := agentSpans(proc)["agent:a"].Data
		if d["ended_by"] != "interruption" {
			t.Errorf("ended_by = %#v, want interruption", d["ended_by"])
		}
		if names, _ := d["pending_tools"].([]string); !slices.Equal(names, []string{"get_weather"}) {
			t.Errorf("pending_tools = %#v, want [get_weather]", d["pending_tools"])
		}
	})

	t.Run("handoff, and the handoff span names its target", func(t *testing.T) {
		billing := &Agent{Name: "billing", ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "paid"))}}}
		triage := &Agent{Name: "triage", Handoffs: []Handoff{HandoffTo(billing)}, ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "transfer_to_billing", "c1", `{}`)),
		}}}
		proc := &recordingProcessor{}
		if _, err := RunSync(context.Background(), triage, "go", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
			t.Fatal(err)
		}
		spans := agentSpans(proc)
		if spans["agent:triage"].Data["ended_by"] != "handoff" || spans["agent:billing"].Data["ended_by"] != "final_output" {
			t.Errorf("ended_by = %#v / %#v, want handoff / final_output", spans["agent:triage"].Data["ended_by"], spans["agent:billing"].Data["ended_by"])
		}
		hs := proc.spansOfType(tracing.SpanTypeHandoff)
		if len(hs) != 1 || hs[0].Data["to_agent"] != "billing" || hs[0].Data["input"] != "{}" {
			t.Fatalf("handoff span = %+v, want to_agent billing and the call's input", hs)
		}
	})

	// A stop lands at the turn boundary, or the model finishes on the very
	// turn it was asked for — the span tells the two apart as RunResult does.
	t.Run("stop", func(t *testing.T) {
		stopped := func(responses ...*ModelResponse) map[string]any {
			t.Helper()
			agent, proc := tracingAgent(t)
			agent.ModelImpl = &fakeModel{responses: responses}
			stream, ctrl := Run(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}})
			ctrl.StopAfterTurn()
			for _, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
			}
			return agentSpans(proc)["agent:a"].Data
		}
		d := stopped(modelResp(functionCallOutput(t, "get_weather", "c1", `{"city":"SF"}`)), modelResp(messageOutput(t, "done")))
		if d["ended_by"] != "stop" {
			t.Errorf("a run stopped at the boundary = %v, want ended_by stop", d)
		}
		d = stopped(modelResp(messageOutput(t, "done")))
		if d["ended_by"] != "final_output" || d["stopped_early"] != true {
			t.Errorf("a run that finished on the stop turn = %v, want final_output with stopped_early", d)
		}
	})
}

// A guardrail span names what it consulted and how each one ruled, and the
// tool stages get a span of their own beside the function span — a Replace
// is otherwise invisible in the trace.
func TestGuardrailSpansRecordVerdicts(t *testing.T) {
	agent, proc := tracingAgent(t)
	agent.Guardrails = []Guardrail{{Name: "pii", Stages: []GuardrailStage{StageInput}, Blocking: true,
		Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
			return Replace("redacted", nil), nil
		}}}
	agent.Tools[0].Guardrails = []Guardrail{
		{Name: "arg_check", Stages: []GuardrailStage{StageToolInput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Allow(nil), nil
			}},
		{Name: "out_check", Stages: []GuardrailStage{StageToolOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Replace("clean", nil), nil
			}},
	}
	agent.ModelImpl = &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "c1", `{"city":"SF"}`)),
		modelResp(messageOutput(t, "done")),
	}}
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
		t.Fatal(err)
	}
	byName := map[string]*tracing.Span{}
	for _, s := range proc.spansOfType(tracing.SpanTypeGuardrail) {
		byName[s.Name] = s
	}
	agentID := proc.spansOfType(tracing.SpanTypeAgent)[0].SpanID
	for name, want := range map[string][2]string{
		"guardrail:input":       {"pii", "replace"},
		"guardrail:tool_input":  {"arg_check", "allow"},
		"guardrail:tool_output": {"out_check", "replace"},
	} {
		s := byName[name]
		if s == nil {
			t.Fatalf("no %s span; got %v", name, slices.Collect(maps.Keys(byName)))
		}
		v, _ := s.Data["guardrails"].([]map[string]any)
		if len(v) != 1 || v[0]["name"] != want[0] || v[0]["action"] != want[1] {
			t.Errorf("%s guardrails = %v, want [{%s %s}]", name, s.Data["guardrails"], want[0], want[1])
		}
		if s.ParentID != agentID {
			t.Errorf("%s hangs under %s, want the agent span", name, s.ParentID)
		}
	}
}

// A generation answered by a fallback says so on its span: the model it names
// is the one configured, not the one that answered.
func TestGenerationSpanRecordsFallbackIndex(t *testing.T) {
	agent, proc := tracingAgent(t)
	backup := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "from backup"))}}
	agent.ModelImpl = NewFallbackModel(&failingModel{err: errors.New("primary down")}, backup)
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc)}}); err != nil {
		t.Fatal(err)
	}
	if d := proc.generationSpans()[0].Data; d["fallback_index"] != 1 {
		t.Errorf("fallback_index = %#v, want 1", d["fallback_index"])
	}

	agent2, proc2 := tracingAgent(t)
	if _, err := RunSync(context.Background(), agent2, "hi", RunOptions{Observe: ObserveOptions{Tracer: tracing.NewTracer(proc2)}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := proc2.generationSpans()[0].Data["fallback_index"]; ok {
		t.Error("fallback_index recorded on a call the primary answered")
	}
}

// The runner's own compaction pass spells its counts the way every other
// compaction span does, so one reader serves them all.
func TestCompactionPointSpanCounts(t *testing.T) {
	c := &recordingCompactor{drop: 2}
	agent, proc := tracingAgent(t)
	if _, err := RunSync(context.Background(), agent, "now", RunOptions{
		Conversation: ConversationOptions{Session: seededSession(t, "one", "two", "three")},
		Compaction:   CompactionOptions{Compactor: c, Points: CompactBeforeRun},
		Observe:      ObserveOptions{Tracer: tracing.NewTracer(proc)},
	}); err != nil {
		t.Fatal(err)
	}
	spans := proc.spansOfType(tracing.SpanTypeCompaction)
	if len(spans) != 1 || spans[0].Data["before_items"] != 3 || spans[0].Data["after_items"] != 1 {
		t.Fatalf("compaction span = %+v, want before_items 3 / after_items 1", spans)
	}
}
