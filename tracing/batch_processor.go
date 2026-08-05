package tracing

import (
	"context"
	"sync"
	"time"
)

// BatchProcessorOptions configures a BatchProcessor. Zero values select sensible
// defaults.
type BatchProcessorOptions struct {
	// MaxBatchSize is the number of items exported per flush. Default 128.
	MaxBatchSize int
	// FlushInterval is how often the background goroutine flushes. Default 5s.
	FlushInterval time.Duration
	// MaxQueueSize bounds the buffer; items beyond it are dropped (and counted).
	// Default 8192.
	MaxQueueSize int
}

// BatchProcessor buffers finished traces and spans and exports them in batches
// from a background goroutine. Span/trace ends are enqueued; a periodic timer
// and a size threshold trigger flushes. Shutdown drains the queue and stops
// the goroutine.
type BatchProcessor struct {
	exporter Exporter
	opts     BatchProcessorOptions

	mu       sync.Mutex
	queue    []Item
	dropped  int
	shutdown bool

	flushNow chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewBatchProcessor starts a BatchProcessor forwarding to exporter. Call
// Shutdown to stop the background goroutine and flush remaining items.
func NewBatchProcessor(exporter Exporter, opts BatchProcessorOptions) *BatchProcessor {
	if opts.MaxBatchSize <= 0 {
		opts.MaxBatchSize = 128
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 5 * time.Second
	}
	if opts.MaxQueueSize <= 0 {
		opts.MaxQueueSize = 8192
	}
	p := &BatchProcessor{
		exporter: exporter,
		opts:     opts,
		flushNow: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	p.wg.Add(1)
	go p.run()
	return p
}

func (p *BatchProcessor) enqueue(item Item) {
	p.mu.Lock()
	if p.shutdown || len(p.queue) >= p.opts.MaxQueueSize {
		p.dropped++
		p.mu.Unlock()
		return
	}
	p.queue = append(p.queue, item)
	full := len(p.queue) >= p.opts.MaxBatchSize
	p.mu.Unlock()
	if full {
		select {
		case p.flushNow <- struct{}{}:
		default:
		}
	}
}

// OnTraceStart enqueues the trace row immediately, so that a crash mid-run does
// not orphan its spans.
func (p *BatchProcessor) OnTraceStart(t *Trace) { p.enqueue(t) }

// OnTraceEnd is a no-op: the trace row was already enqueued on start.
func (p *BatchProcessor) OnTraceEnd(*Trace) {}

// OnSpanStart is a no-op: only span ends carry the final data.
func (p *BatchProcessor) OnSpanStart(*Span) {}

// OnSpanEnd enqueues the finished span.
func (p *BatchProcessor) OnSpanEnd(s *Span) { p.enqueue(s) }

func (p *BatchProcessor) run() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.opts.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.flush()
		case <-p.flushNow:
			p.flush()
		case <-p.done:
			p.flush() // final drain
			return
		}
	}
}

// flush exports all currently-queued items in batches.
func (p *BatchProcessor) flush() {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		n := min(len(p.queue), p.opts.MaxBatchSize)
		batch := make([]Item, n)
		copy(batch, p.queue[:n])
		p.queue = p.queue[n:]
		p.mu.Unlock()
		if p.exporter != nil {
			p.exporter.Export(batch)
		}
	}
}

// ForceFlush exports all currently-queued items synchronously.
func (p *BatchProcessor) ForceFlush() { p.flush() }

// Shutdown stops the background goroutine after a final flush. It honors ctx: if
// the context is cancelled first, it returns without waiting for the goroutine.
// Items arriving after Shutdown are dropped, and counted: telemetry that
// arrives once the exporter is gone has nowhere to go, and blocking a caller
// on a shut-down processor would be worse than losing a span.
func (p *BatchProcessor) Shutdown(ctx context.Context) {
	p.mu.Lock()
	p.shutdown = true
	p.mu.Unlock()
	p.stopOnce.Do(func() { close(p.done) })
	waited := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
	}
}

// Dropped returns the number of items dropped due to a full queue or because
// they arrived after Shutdown.
func (p *BatchProcessor) Dropped() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

var _ Processor = (*BatchProcessor)(nil)
