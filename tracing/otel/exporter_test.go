package otel

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/zzir/agents-go/tracing"
)

// newTestExporter wires a recorder so a test can read the OTel spans back.
func newTestExporter(t *testing.T) (*Exporter, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp, exp, err := NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	if err != nil {
		t.Fatalf("NewTracerProvider: %v", err)
	}
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exp, rec
}

func span(traceID, spanID, parentID, typ, name string, data map[string]any) *tracing.Span {
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	return &tracing.Span{
		TraceID:   traceID,
		SpanID:    spanID,
		ParentID:  parentID,
		Type:      typ,
		Name:      name,
		StartedAt: start,
		EndedAt:   start.Add(250 * time.Millisecond),
		Data:      data,
	}
}

const (
	traceA   = "trace_0af7651916cd43dd8448eb211c80319c"
	spanRoot = "span_b9c7c989f97918e1"
	spanKid  = "span_b7ad6b7169203331"
	spanKid2 = "span_b7ad6b7169203332"
)

// The whole reason this package exists: our spans are flat records exported
// after they finish, usually children first. The tree must survive that.
//
// The naive approach (inject a remote parent, let the SDK mint ids) links
// children but hands each root a fresh trace id, splitting the trace in two.
func TestRebuildsTreeWithChildrenExportedFirst(t *testing.T) {
	exp, rec := newTestExporter(t)

	// Deliberately out of order: both children, then their parent.
	exp.Export([]tracing.Item{
		&tracing.Trace{TraceID: traceA, WorkflowName: "wf", GroupID: "g1"},
		span(traceA, spanKid, spanRoot, tracing.SpanTypeFunction, "", map[string]any{"name": "get_weather", "call_id": "c1"}),
		span(traceA, spanKid2, spanRoot, tracing.SpanTypeGeneration, "", map[string]any{"model": "gpt-4o"}),
		span(traceA, spanRoot, "", tracing.SpanTypeAgent, "", map[string]any{"name": "assistant"}),
	})

	got := rec.Ended()
	if len(got) != 3 {
		t.Fatalf("exported %d spans, want 3", len(got))
	}

	wantTrace := traceA[len("trace_"):]
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range got {
		byName[s.Name()] = s
		if h := s.SpanContext().TraceID().String(); h != wantTrace {
			t.Errorf("span %q has trace id %s, want %s — the trace was split", s.Name(), h, wantTrace)
		}
	}

	root, ok := byName["invoke_agent assistant"]
	if !ok {
		t.Fatalf("no root span; got %v", names(got))
	}
	if root.Parent().IsValid() {
		t.Errorf("root span has parent %s, want none", root.Parent().SpanID())
	}
	if h := root.SpanContext().SpanID().String(); h != spanRoot[len("span_"):] {
		t.Errorf("root span id = %s, want the id we assigned (%s)", h, spanRoot[len("span_"):])
	}

	for _, child := range []string{"execute_tool get_weather", "chat gpt-4o"} {
		s, ok := byName[child]
		if !ok {
			t.Fatalf("missing %q; got %v", child, names(got))
		}
		if p := s.Parent().SpanID().String(); p != spanRoot[len("span_"):] {
			t.Errorf("%q parent = %s, want %s", child, p, spanRoot[len("span_"):])
		}
	}
}

// Span names and attributes follow the GenAI semantic conventions where they
// apply; our own concepts get an agents. prefix rather than a fake gen_ai one.
func TestSemanticConventions(t *testing.T) {
	exp, rec := newTestExporter(t)
	exp.Export([]tracing.Item{
		span(traceA, spanRoot, "", tracing.SpanTypeGeneration, "", map[string]any{
			"model": "gpt-4o", "response_id": "resp_1",
			"input_tokens": 12, "output_tokens": float64(34), // float64: a JSON round-trip
		}),
		span(traceA, spanKid, spanRoot, tracing.SpanTypeHandoff, "", map[string]any{
			"name": "transfer_to_billing",
		}),
		span(traceA, spanKid2, spanRoot, tracing.SpanTypeGuardrail, "", map[string]any{
			"stage": "input",
		}),
	})

	got := indexByName(rec.Ended())

	chat := got["chat gpt-4o"]
	if chat == nil {
		t.Fatalf("no chat span; got %v", names(rec.Ended()))
	}
	assertAttr(t, chat, attrOperationName, "chat")
	assertAttr(t, chat, attrProviderName, "openai")
	assertAttr(t, chat, attrRequestModel, "gpt-4o")
	assertAttr(t, chat, attrResponseID, "resp_1")
	assertInt(t, chat, attrUsageInputTokens, 12)
	assertInt(t, chat, attrUsageOutputTokens, 34)

	handoff := got["handoff"]
	if handoff == nil {
		t.Fatal("no handoff span")
	}
	assertAttr(t, handoff, attrHandoffTool, "transfer_to_billing")

	guard := got["guardrail"]
	if guard == nil {
		t.Fatal("no guardrail span")
	}
	assertAttr(t, guard, attrGuardrailStage, "input")
}

// A generation span is built with "name" (the agent's configured model) and
// annotated with "model" (what was actually called). They differ whenever
// RunOptions.Model.Override is set, and the one that was called is the truthful
// answer for gen_ai.request.model.
func TestGenerationPrefersTheModelActuallyCalled(t *testing.T) {
	exp, rec := newTestExporter(t)
	exp.Export([]tracing.Item{
		span(traceA, spanRoot, "", tracing.SpanTypeGeneration, "", map[string]any{
			"name": "gpt-4o", "model": "gpt-4o-mini",
		}),
		span(traceA, spanKid, "", tracing.SpanTypeGeneration, "", map[string]any{
			"name": "gpt-4o", // no override recorded: fall back
		}),
	})
	got := indexByName(rec.Ended())
	if got["chat gpt-4o-mini"] == nil {
		t.Errorf("span names = %v, want the called model to win", names(rec.Ended()))
	}
	if got["chat gpt-4o"] == nil {
		t.Errorf("span names = %v, want a fallback to the configured model", names(rec.Ended()))
	}
}

// The workflow name belongs on the root span only — repeating it on every child
// would multiply a constant across the whole trace.
func TestWorkflowAttributesOnRootOnly(t *testing.T) {
	exp, rec := newTestExporter(t)
	exp.Export([]tracing.Item{
		&tracing.Trace{TraceID: traceA, WorkflowName: "my workflow", GroupID: "thread-7"},
		span(traceA, spanRoot, "", tracing.SpanTypeAgent, "", map[string]any{"name": "a"}),
		span(traceA, spanKid, spanRoot, tracing.SpanTypeFunction, "", map[string]any{"name": "t"}),
	})
	got := indexByName(rec.Ended())

	assertAttr(t, got["invoke_agent a"], attrWorkflowName, "my workflow")
	assertAttr(t, got["invoke_agent a"], attrTraceGroupID, "thread-7")
	if has(got["execute_tool t"], attrWorkflowName) {
		t.Error("workflow name repeated on a child span")
	}
}

// A span whose trace record never arrived still exports — losing the workflow
// name is better than losing the span.
func TestSpanWithoutTraceRecordStillExports(t *testing.T) {
	exp, rec := newTestExporter(t)
	exp.Export([]tracing.Item{
		span(traceA, spanRoot, "", tracing.SpanTypeAgent, "", map[string]any{"name": "a"}),
	})
	if n := len(rec.Ended()); n != 1 {
		t.Fatalf("exported %d spans, want 1", n)
	}
	if has(rec.Ended()[0], attrWorkflowName) {
		t.Error("workflow name appeared without a trace record")
	}
}

func TestErrorMapping(t *testing.T) {
	exp, rec := newTestExporter(t)
	s := span(traceA, spanRoot, "", tracing.SpanTypeFunction, "", map[string]any{"name": "t"})
	s.Error = &tracing.SpanError{Message: "boom", Data: map[string]any{"code": "tool_timeout"}}
	exp.Export([]tracing.Item{s})

	got := rec.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", got.Status().Code)
	}
	if got.Status().Description != "boom" {
		t.Errorf("status description = %q", got.Status().Description)
	}
	// error.type carries the SDK's ErrorCode, which is exactly the stable,
	// low-cardinality classification the convention asks for.
	assertAttr(t, got, attrErrorType, "tool_timeout")

	// Without a code it falls back to the convention's placeholder rather than
	// inventing a high-cardinality value from the message.
	exp2, rec2 := newTestExporter(t)
	s2 := span(traceA, spanRoot, "", tracing.SpanTypeFunction, "", map[string]any{"name": "t"})
	s2.Error = &tracing.SpanError{Message: "boom"}
	exp2.Export([]tracing.Item{s2})
	assertAttr(t, rec2.Ended()[0], attrErrorType, "_OTHER")
}

// Timestamps come from the original span, not from when the batch happened to
// be flushed — otherwise every duration would measure queue latency.
func TestPreservesTimestamps(t *testing.T) {
	exp, rec := newTestExporter(t)
	s := span(traceA, spanRoot, "", tracing.SpanTypeAgent, "", map[string]any{"name": "a"})
	exp.Export([]tracing.Item{s})

	got := rec.Ended()[0]
	if !got.StartTime().Equal(s.StartedAt) {
		t.Errorf("start = %v, want %v", got.StartTime(), s.StartedAt)
	}
	if !got.EndTime().Equal(s.EndedAt) {
		t.Errorf("end = %v, want %v", got.EndTime(), s.EndedAt)
	}
	if d := got.EndTime().Sub(got.StartTime()); d != 250*time.Millisecond {
		t.Errorf("duration = %v, want 250ms", d)
	}
}

// A span that never finished (the process died mid-run) still exports, closed
// at its start. Giving it "now" would report a duration equal to how long the
// batch sat in the queue.
func TestUnfinishedSpanExportsWithZeroDuration(t *testing.T) {
	exp, rec := newTestExporter(t)
	s := span(traceA, spanRoot, "", tracing.SpanTypeAgent, "", map[string]any{"name": "a"})
	s.EndedAt = time.Time{}
	exp.Export([]tracing.Item{s})

	got := rec.Ended()
	if len(got) != 1 {
		t.Fatalf("an unfinished span was dropped")
	}
	if d := got[0].EndTime().Sub(got[0].StartTime()); d != 0 {
		t.Errorf("duration = %v, want 0", d)
	}
}

// An id this exporter did not mint is skipped rather than exported under a
// fabricated one, which would attach the span to an unrelated trace.
func TestForeignIDsAreSkipped(t *testing.T) {
	exp, rec := newTestExporter(t)
	exp.Export([]tracing.Item{
		span("not-a-trace-id", spanRoot, "", tracing.SpanTypeAgent, "", nil),
		span(traceA, "not-a-span-id", "", tracing.SpanTypeAgent, "", nil),
		span(traceA, spanRoot, "", tracing.SpanTypeAgent, "", map[string]any{"name": "ok"}),
	})
	got := rec.Ended()
	if len(got) != 1 || got[0].Name() != "invoke_agent ok" {
		t.Fatalf("exported %v, want only the valid span", names(got))
	}
}

// An unparsable parent must not silently reparent the span to the root — it is
// dropped as a parent, and the span still exports.
func TestUnparsableParentDoesNotReparent(t *testing.T) {
	exp, rec := newTestExporter(t)
	exp.Export([]tracing.Item{
		span(traceA, spanRoot, "garbage", tracing.SpanTypeAgent, "", map[string]any{"name": "a"}),
	})
	got := rec.Ended()
	if len(got) != 1 {
		t.Fatalf("span with a bad parent was dropped")
	}
	if got[0].Parent().IsValid() {
		t.Errorf("bad parent %q produced a valid parent link", "garbage")
	}
}

func TestNewExporterValidatesOptions(t *testing.T) {
	if _, err := NewExporter(Options{}); err == nil {
		t.Error("a missing TracerProvider must be an error, not a silently-dropping exporter")
	}
	tp := sdktrace.NewTracerProvider()
	if _, err := NewExporter(Options{TracerProvider: tp}); err == nil {
		t.Error("a missing IDGenerator must be an error")
	}
}

// The generator falls back to random ids when nothing is pinned, so a provider
// built with it stays usable for ordinary instrumentation.
func TestIDGeneratorFallsBackToRandom(t *testing.T) {
	g := NewIDGenerator()
	t1, s1 := g.NewIDs(context.Background())
	t2, s2 := g.NewIDs(context.Background())
	if t1 == t2 || s1 == s2 {
		t.Error("unpinned generation repeated an id")
	}
	if !t1.IsValid() || !s1.IsValid() {
		t.Error("unpinned generation produced an invalid id")
	}

	// A pin is consumed by exactly one call.
	want, _ := parseTraceID(traceA)
	wantSpan, _ := parseSpanID(spanRoot)
	g.pin(want, wantSpan)
	gotT, gotS := g.NewIDs(context.Background())
	if gotT != want || gotS != wantSpan {
		t.Error("pinned ids were not returned")
	}
	if gotT2, _ := g.NewIDs(context.Background()); gotT2 == want {
		t.Error("the pin survived past one span")
	}
}

func TestIDWidthsMatchOTel(t *testing.T) {
	// tracing.NewSpanID must stay 8 bytes; a wider id would silently truncate
	// here and collapse distinct spans onto one id.
	id, err := parseSpanID(tracing.NewSpanID())
	if err != nil {
		t.Fatalf("tracing.NewSpanID is no longer an OTel-width span id: %v", err)
	}
	if !id.IsValid() {
		t.Error("parsed span id is invalid")
	}
	tid, err := parseTraceID(tracing.NewTraceID())
	if err != nil {
		t.Fatalf("tracing.NewTraceID is no longer an OTel-width trace id: %v", err)
	}
	if !tid.IsValid() {
		t.Error("parsed trace id is invalid")
	}
}

// End to end through the batch processor, which is the only supported way to
// drive this exporter.
func TestThroughBatchProcessor(t *testing.T) {
	exp, rec := newTestExporter(t)
	proc := tracing.NewBatchProcessor(exp, tracing.BatchProcessorOptions{})
	tracer := tracing.NewTracer(proc)

	tr := tracer.StartTrace("wf")
	agent := tr.StartAgentSpan("assistant", "")
	gen := tr.StartGenerationSpan("gpt-4o", agent.Span.SpanID)
	gen.Finish()
	agent.Finish()
	tr.Finish()
	proc.ForceFlush()

	got := rec.Ended()
	if len(got) != 2 {
		t.Fatalf("exported %d spans, want 2: %v", len(got), names(got))
	}
	idx := indexByName(got)
	if idx["invoke_agent assistant"] == nil || idx["chat gpt-4o"] == nil {
		t.Fatalf("unexpected span names: %v", names(got))
	}
	if p := idx["chat gpt-4o"].Parent().SpanID(); p != idx["invoke_agent assistant"].SpanContext().SpanID() {
		t.Error("the generation span is not parented under the agent span")
	}
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}

func indexByName(spans []sdktrace.ReadOnlySpan) map[string]sdktrace.ReadOnlySpan {
	out := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		out[s.Name()] = s
	}
	return out
}

func attrOf(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	if s == nil {
		return attribute.Value{}, false
	}
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func has(s sdktrace.ReadOnlySpan, key string) bool {
	_, ok := attrOf(s, key)
	return ok
}

func assertAttr(t *testing.T, s sdktrace.ReadOnlySpan, key, want string) {
	t.Helper()
	v, ok := attrOf(s, key)
	if !ok {
		t.Errorf("missing attribute %s", key)
		return
	}
	if v.AsString() != want {
		t.Errorf("%s = %q, want %q", key, v.AsString(), want)
	}
}

func assertInt(t *testing.T, s sdktrace.ReadOnlySpan, key string, want int64) {
	t.Helper()
	v, ok := attrOf(s, key)
	if !ok {
		t.Errorf("missing attribute %s", key)
		return
	}
	if v.AsInt64() != want {
		t.Errorf("%s = %d, want %d", key, v.AsInt64(), want)
	}
}

// A trace's metadata exists to stamp its root span. Holding it afterwards
// makes the map grow with every run a long-lived server executes.
func TestExporterReleasesTraceMetadataAtTheRoot(t *testing.T) {
	tp, exp, err := NewTracerProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tr := &tracing.Trace{TraceID: tracing.NewTraceID(), WorkflowName: "w"}
	root := &tracing.Span{
		TraceID: tr.TraceID, SpanID: tracing.NewSpanID(),
		Type: "agent", StartedAt: time.Now(), EndedAt: time.Now(),
	}
	exp.Export([]tracing.Item{tr, root})

	exp.mu.Lock()
	held := len(exp.traces)
	exp.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d trace record(s) still held after the root span exported", held)
	}
}

// A trace whose root never arrives — an abandoned run, or a root dropped by a
// full queue — would otherwise be immortal.
func TestExporterBoundsTracesWithNoRoot(t *testing.T) {
	tp, exp, err := NewTracerProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	for i := 0; i < maxPendingTraces+100; i++ {
		exp.Export([]tracing.Item{&tracing.Trace{TraceID: tracing.NewTraceID(), WorkflowName: "w"}})
	}

	exp.mu.Lock()
	held := len(exp.traces)
	exp.mu.Unlock()
	if held > maxPendingTraces {
		t.Fatalf("holding %d trace records, ceiling is %d", held, maxPendingTraces)
	}
}
