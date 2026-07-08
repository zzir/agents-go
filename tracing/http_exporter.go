package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// HTTPExporter ships finished traces and spans to an HTTP endpoint as a JSON
// batch. It is a generic exporter suitable for custom trace collectors.
//
// Note: it posts this package's generic Trace/Span JSON, not OpenAI's
// proprietary span-data schema, so it does not populate the OpenAI traces
// dashboard. Point it at a collector that understands the generic shape, or wrap
// it to translate.
type HTTPExporter struct {
	endpoint string
	client   *http.Client
	headers  map[string]string

	mu      sync.Mutex
	dropped int
}

// HTTPExporterOptions configures an HTTPExporter.
type HTTPExporterOptions struct {
	// Headers are added to every request (e.g. Authorization).
	Headers map[string]string
	// Client overrides the default HTTP client (which has a 30s timeout).
	Client *http.Client
}

// NewHTTPExporter returns an exporter that POSTs batches to endpoint.
func NewHTTPExporter(endpoint string, opts HTTPExporterOptions) *HTTPExporter {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPExporter{endpoint: endpoint, client: client, headers: opts.Headers}
}

// httpPayload is the JSON body posted by the exporter, splitting items into
// traces and spans for easier consumption.
type httpPayload struct {
	Traces []*Trace `json:"traces,omitempty"`
	Spans  []*Span  `json:"spans,omitempty"`
}

// Export implements Exporter. Failed batches — including non-2xx responses —
// are dropped without retry (telemetry should never break the application) and
// counted (see Dropped); wrap the exporter to add logging or retries.
func (e *HTTPExporter) Export(items []Item) {
	if len(items) == 0 {
		return
	}
	var payload httpPayload
	for _, item := range items {
		switch v := item.(type) {
		case *Trace:
			payload.Traces = append(payload.Traces, v)
		case *Span:
			payload.Spans = append(payload.Spans, v)
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		e.drop(len(items))
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		e.drop(len(items))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.drop(len(items))
		return
	}
	// Drain the body so the keep-alive connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		e.drop(len(items))
	}
}

// drop counts items lost to a failed export.
func (e *HTTPExporter) drop(n int) {
	e.mu.Lock()
	e.dropped += n
	e.mu.Unlock()
}

// Dropped returns the number of items dropped due to failed exports.
func (e *HTTPExporter) Dropped() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

var _ Exporter = (*HTTPExporter)(nil)
