package tracing

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"
)

func TestNewTraceID_Format(t *testing.T) {
	re := regexp.MustCompile(`^trace_[0-9a-f]{32}$`)
	id1, id2 := NewTraceID(), NewTraceID()
	if !re.MatchString(id1) {
		t.Errorf("trace ID %q does not match trace_<32 hex>", id1)
	}
	if id1 == id2 {
		t.Errorf("two trace IDs collided: %q", id1)
	}
}

func TestNewSpanID_Format(t *testing.T) {
	re := regexp.MustCompile(`^span_[0-9a-f]{24}$`)
	id1, id2 := NewSpanID(), NewSpanID()
	if !re.MatchString(id1) {
		t.Errorf("span ID %q does not match span_<24 hex>", id1)
	}
	if id1 == id2 {
		t.Errorf("two span IDs collided: %q", id1)
	}
}

func TestBatchProcessor_ShutdownDrains(t *testing.T) {
	col := &CollectingExporter{}
	// Long flush interval so only the shutdown drain exports.
	p := NewBatchProcessor(col, BatchProcessorOptions{FlushInterval: time.Hour, MaxBatchSize: 100})

	for range 10 {
		p.OnSpanEnd(&Span{SpanID: "s"})
	}
	if col.Len() != 0 {
		t.Fatalf("nothing should export before flush, got %d", col.Len())
	}
	p.Shutdown(context.Background())
	if col.Len() != 10 {
		t.Errorf("shutdown should drain all 10 items, got %d", col.Len())
	}
}

func TestBatchProcessor_ForceFlush(t *testing.T) {
	col := &CollectingExporter{}
	p := NewBatchProcessor(col, BatchProcessorOptions{FlushInterval: time.Hour})
	defer p.Shutdown(context.Background())

	p.OnTraceStart(&Trace{TraceID: "t"})
	p.OnSpanEnd(&Span{SpanID: "s"})
	p.ForceFlush()
	if col.Len() != 2 {
		t.Errorf("ForceFlush should export 2 items, got %d", col.Len())
	}
}

func TestBatchProcessor_ExportsTraceOnStart(t *testing.T) {
	col := &CollectingExporter{}
	p := NewBatchProcessor(col, BatchProcessorOptions{FlushInterval: time.Hour})
	defer p.Shutdown(context.Background())

	tr := NewTracer(p)
	trace := tr.StartTrace("wf")
	// The trace row must be exportable before the trace finishes, so a crash
	// mid-run does not orphan its spans.
	p.ForceFlush()
	items := col.Items()
	if len(items) != 1 {
		t.Fatalf("expected the trace row exported at start, got %d items", len(items))
	}
	if got, ok := items[0].(*Trace); !ok || got.WorkflowName != "wf" {
		t.Errorf("exported item = %+v, want the started trace", items[0])
	}
	// Finishing must not enqueue the trace row a second time.
	trace.Finish()
	p.ForceFlush()
	if col.Len() != 1 {
		t.Errorf("OnTraceEnd should not re-export the trace, got %d items", col.Len())
	}
}

func TestBatchProcessor_DropsAfterShutdown(t *testing.T) {
	col := &CollectingExporter{}
	p := NewBatchProcessor(col, BatchProcessorOptions{FlushInterval: time.Hour})
	p.Shutdown(context.Background())

	p.OnSpanEnd(&Span{SpanID: "late"})
	p.OnTraceStart(&Trace{TraceID: "late"})
	if p.Dropped() != 2 {
		t.Errorf("post-shutdown items should be dropped, Dropped() = %d", p.Dropped())
	}
	p.mu.Lock()
	qlen := len(p.queue)
	p.mu.Unlock()
	if qlen != 0 {
		t.Errorf("post-shutdown items should not be queued, queue len = %d", qlen)
	}
	p.ForceFlush()
	if col.Len() != 0 {
		t.Errorf("post-shutdown items should not be exported, got %d", col.Len())
	}
}

func TestBatchProcessor_ThresholdFlush(t *testing.T) {
	var mu sync.Mutex
	exported := 0
	exp := FuncExporter(func(items []any) {
		mu.Lock()
		exported += len(items)
		mu.Unlock()
	})
	p := NewBatchProcessor(exp, BatchProcessorOptions{FlushInterval: time.Hour, MaxBatchSize: 5})
	defer p.Shutdown(context.Background())

	for range 5 {
		p.OnSpanEnd(&Span{SpanID: "s"})
	}
	// Threshold flush happens asynchronously; poll briefly.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := exported
		mu.Unlock()
		if n == 5 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("threshold flush did not export 5 items, got %d", exported)
}

func TestBatchProcessor_DropsWhenQueueFull(t *testing.T) {
	p := NewBatchProcessor(&CollectingExporter{}, BatchProcessorOptions{
		FlushInterval: time.Hour, MaxQueueSize: 3, MaxBatchSize: 100,
	})
	defer p.Shutdown(context.Background())
	for range 10 {
		p.OnSpanEnd(&Span{SpanID: "s"})
	}
	if p.Dropped() != 7 {
		t.Errorf("expected 7 dropped (10 - queue 3), got %d", p.Dropped())
	}
}

func TestTracer_SpanHierarchy(t *testing.T) {
	col := &CollectingExporter{}
	p := NewBatchProcessor(col, BatchProcessorOptions{FlushInterval: time.Hour})
	tr := NewTracer(p)

	trace := tr.StartTrace("test-workflow")
	span := trace.StartSpan("agent", "")
	child := span.StartSpan("generation")
	child.Set("model", "gpt-4o")
	child.Finish()
	span.Finish()
	trace.Finish()

	p.Shutdown(context.Background())

	items := col.Items()
	if len(items) != 3 {
		t.Fatalf("expected 3 items (2 spans + 1 trace), got %d", len(items))
	}
	// The child span should reference the parent span ID.
	var childSpan *Span
	for _, it := range items {
		if s, ok := it.(*Span); ok && s.Name == "generation" {
			childSpan = s
		}
	}
	if childSpan == nil || childSpan.ParentID == "" {
		t.Errorf("child span missing parent reference: %+v", childSpan)
	}
	if childSpan.Data["model"] != "gpt-4o" {
		t.Errorf("span data not recorded: %+v", childSpan.Data)
	}
	if childSpan.EndedAt.IsZero() {
		t.Error("child span not stamped with end time")
	}
}

func TestNilTracerIsNoop(t *testing.T) {
	var tr *Tracer
	// Must not panic.
	h := tr.StartTrace("x")
	s := h.StartSpan("y", "")
	s.Set("k", "v")
	s.Finish()
	h.Finish()
}
