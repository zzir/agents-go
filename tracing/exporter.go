package tracing

import (
	"encoding/json"
	"io"
	"sync"
)

// NoopExporter discards all items. It is the default when tracing is enabled
// without a configured destination.
type NoopExporter struct{}

func (NoopExporter) Export([]any) {}

// FuncExporter adapts a function to the Exporter interface, convenient for tests
// and custom sinks.
type FuncExporter func(items []any)

func (f FuncExporter) Export(items []any) { f(items) }

// ConsoleExporter writes each item as a line of JSON to an io.Writer. It is
// goroutine-safe.
type ConsoleExporter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewConsoleExporter returns a ConsoleExporter writing to w.
func NewConsoleExporter(w io.Writer) *ConsoleExporter { return &ConsoleExporter{w: w} }

func (e *ConsoleExporter) Export(items []any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	enc := json.NewEncoder(e.w)
	for _, item := range items {
		_ = enc.Encode(item)
	}
}

// CollectingExporter accumulates exported items in memory for inspection, mainly
// for tests. It is goroutine-safe.
type CollectingExporter struct {
	mu    sync.Mutex
	items []any
}

func (e *CollectingExporter) Export(items []any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.items = append(e.items, items...)
}

// Items returns a snapshot of all collected items.
func (e *CollectingExporter) Items() []any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]any(nil), e.items...)
}

// Len returns the number of collected items.
func (e *CollectingExporter) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.items)
}
