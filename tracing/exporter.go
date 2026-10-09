package tracing

import (
	"encoding/json"
	"io"
	"sync"
)

// Item is the sealed union an Exporter receives: a *Trace or a *Span, so a type
// switch over it is exhaustive.
type Item interface {
	isTraceItem()
}

// FuncExporter adapts a function to the Exporter interface.
type FuncExporter func(items []Item)

// Export implements Exporter.
func (f FuncExporter) Export(items []Item) { f(items) }

// ConsoleExporter writes each item as a line of JSON to an io.Writer; safe for
// concurrent use.
type ConsoleExporter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewConsoleExporter returns a ConsoleExporter writing to w.
func NewConsoleExporter(w io.Writer) *ConsoleExporter { return &ConsoleExporter{w: w} }

// Export implements Exporter.
func (e *ConsoleExporter) Export(items []Item) {
	e.mu.Lock()
	defer e.mu.Unlock()
	enc := json.NewEncoder(e.w)
	for _, item := range items {
		_ = enc.Encode(item)
	}
}

// CollectingExporter accumulates exported items in memory; safe for concurrent use.
type CollectingExporter struct {
	mu    sync.Mutex
	items []Item
}

// Export implements Exporter.
func (e *CollectingExporter) Export(items []Item) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.items = append(e.items, items...)
}

// Items returns a snapshot of all collected items.
func (e *CollectingExporter) Items() []Item {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Item(nil), e.items...)
}

// Len returns the number of collected items.
func (e *CollectingExporter) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.items)
}
