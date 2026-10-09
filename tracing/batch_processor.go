package tracing

import (
	"context"
	"sync"
	"time"
)

// BatchProcessorOptions configures a BatchProcessor; zero values select the defaults.
type BatchProcessorOptions struct {
	// MaxBatchSize is the number of items exported per flush. Default 128.
	MaxBatchSize int
	// FlushInterval is how often the background goroutine flushes. Default 5s.
	FlushInterval time.Duration
	// MaxQueueSize bounds the buffer; items beyond it are dropped and counted.
	// Default 8192.
	MaxQueueSize int
	// OnDrop, when set, is called on every dropped item with the running total
	// (spec §2.11e). It runs unlocked on the dropping goroutine: totals may
	// arrive out of order; keep it fast.
	OnDrop func(dropped int)
}

// BatchProcessor buffers started traces and finished spans and exports them in
// batches from a background goroutine, on a timer and on a size threshold.
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

// NewBatchProcessor starts a BatchProcessor forwarding to exporter; Shutdown
// stops it after a final flush.
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
	p.wg.Go(p.run)
	return p
}

func (p *BatchProcessor) enqueue(item Item) {
	p.mu.Lock()
	if p.shutdown || len(p.queue) >= p.opts.MaxQueueSize {
		p.dropped++
		dropped := p.dropped
		p.mu.Unlock()
		if p.opts.OnDrop != nil {
			p.opts.OnDrop(dropped)
		}
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

// OnTraceStart enqueues the trace at once, so a crash mid-run cannot orphan its spans.
func (p *BatchProcessor) OnTraceStart(t *Trace) { p.enqueue(t) }

// OnTraceEnd is a no-op: the trace row was already enqueued on start.
func (p *BatchProcessor) OnTraceEnd(*Trace) {}

// OnSpanStart is a no-op: only span ends carry the final data.
func (p *BatchProcessor) OnSpanStart(*Span) {}

// OnSpanEnd enqueues the finished span.
func (p *BatchProcessor) OnSpanEnd(s *Span) { p.enqueue(s) }

func (p *BatchProcessor) run() {
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
		// clear releases the exported items; reslicing alone keeps them reachable.
		clear(p.queue[:n])
		p.queue = p.queue[n:]
		p.mu.Unlock()
		if p.exporter != nil {
			p.exporter.Export(batch)
		}
	}
}

// ForceFlush exports all currently-queued items synchronously.
func (p *BatchProcessor) ForceFlush() { p.flush() }

// Shutdown stops the background goroutine after a final flush, returning early
// if ctx ends first; later items are dropped and counted like any other.
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

// Dropped returns the number of items dropped by a full queue or after Shutdown.
func (p *BatchProcessor) Dropped() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

var _ Processor = (*BatchProcessor)(nil)
