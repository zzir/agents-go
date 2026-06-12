package tracing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHTTPExporter(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := NewHTTPExporter(srv.URL, HTTPExporterOptions{Headers: map[string]string{"Authorization": "Bearer tok"}})
	proc := NewBatchProcessor(exp, BatchProcessorOptions{FlushInterval: time.Hour})

	tr := NewTracer(proc)
	trace := tr.StartTrace("wf")
	span := trace.StartSpan("agent", "")
	span.Finish()
	trace.Finish()
	proc.Shutdown(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
	var payload httpPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("decode posted body: %v (%s)", err, gotBody)
	}
	if len(payload.Traces) != 1 {
		t.Errorf("traces posted = %d, want 1", len(payload.Traces))
	}
	if len(payload.Spans) != 1 || payload.Spans[0].Name != "agent" {
		t.Errorf("spans posted = %+v", payload.Spans)
	}
	if exp.Dropped() != 0 {
		t.Errorf("successful export should not count drops, got %d", exp.Dropped())
	}
}

func TestHTTPExporter_Non2xxDropsBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	exp := NewHTTPExporter(srv.URL, HTTPExporterOptions{})
	exp.Export([]any{&Trace{TraceID: "t"}, &Span{SpanID: "s"}})
	if exp.Dropped() != 2 {
		t.Errorf("non-2xx response should drop the batch, Dropped() = %d", exp.Dropped())
	}
}
