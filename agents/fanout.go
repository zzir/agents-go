package agents

import (
	"fmt"
	"iter"
	"sync"
)

// Seq pairs a broadcast value with its position in the stream, monotonic per Fanout.
type Seq[T any] struct {
	Seq   int
	Value T
}

// GapError reports that a subscriber fell behind and items were dropped,
// delivered in-band before the next item that got through; resubscribe from
// LastGood — see spec §2.11.
type GapError struct {
	// Dropped is how many items were discarded.
	Dropped int
	// LastGood is the sequence number of the last item delivered before the gap.
	LastGood int
	// Next is the sequence number of the item after the gap, or 0 when the
	// gap runs to the end of the stream (the item beside it is the zero value).
	Next int
}

func (e *GapError) Error() string {
	if e.Next == 0 {
		return fmt.Sprintf("fanout: dropped %d item(s) after seq %d and the stream ended (subscriber too slow)",
			e.Dropped, e.LastGood)
	}
	return fmt.Sprintf("fanout: dropped %d item(s) between seq %d and %d (subscriber too slow)",
		e.Dropped, e.LastGood, e.Next)
}

// AtEnd reports whether the gap runs to the end of the stream.
func (e *GapError) AtEnd() bool { return e.Next == 0 }

// FanoutOptions configures a Fanout. The zero value is usable.
type FanoutOptions struct {
	// Subscriber is the per-subscriber buffer, in items, past which that
	// subscriber drops. Defaults to DefaultSubscriberBuffer.
	Subscriber int

	// Replay is how many recent items to retain for late or reattaching
	// subscribers; zero disables replay.
	Replay int
}

const (
	// DefaultSubscriberBuffer is the per-subscriber buffer when unset.
	DefaultSubscriberBuffer = 256
)

// Fanout broadcasts one producer's items to many independent subscribers;
// Publish never blocks and a full subscriber loses items with a *GapError —
// see spec §2.11. Safe for concurrent use.
type Fanout[T any] struct {
	opts FanoutOptions

	// pubMu serializes a publish end to end. Lock order: pubMu before mu; mu
	// is never held in delivery.
	pubMu sync.Mutex

	mu     sync.Mutex
	seq    int
	replay []Seq[T]
	subs   map[int]*subscriber[T]
	nextID int
	closed bool
}

type subscriber[T any] struct {
	id int
	ch chan delivery[T]
	// done closes when this subscriber detaches, finished when the producer
	// is done; ch itself is never closed (a concurrent Publish may send).
	done     chan struct{}
	finished chan struct{}

	// mu guards the drop bookkeeping.
	mu       sync.Mutex
	dropped  int
	lastGood int
}

// delivery carries an item plus the gap (if any) that immediately precedes it.
type delivery[T any] struct {
	item Seq[T]
	gap  *GapError
}

// NewFanout creates a Fanout; Close it when the producer is done.
func NewFanout[T any](opts FanoutOptions) *Fanout[T] {
	if opts.Subscriber <= 0 {
		opts.Subscriber = DefaultSubscriberBuffer
	}
	return &Fanout[T]{opts: opts, subs: make(map[int]*subscriber[T])}
}

// Publish assigns the next sequence number and delivers to every subscriber
// without blocking; a no-op after Close.
func (f *Fanout[T]) Publish(v T) {
	f.pubMu.Lock()
	defer f.pubMu.Unlock()

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.seq++
	item := Seq[T]{Seq: f.seq, Value: v}
	if f.opts.Replay > 0 {
		f.replay = append(f.replay, item)
		if len(f.replay) > f.opts.Replay {
			f.replay = f.replay[len(f.replay)-f.opts.Replay:]
		}
	}
	subs := make([]*subscriber[T], 0, len(f.subs))
	for _, s := range f.subs {
		subs = append(subs, s)
	}
	f.mu.Unlock()

	// Delivery happens outside mu, still under pubMu.
	for _, s := range subs {
		s.deliver(item)
	}
}

// deliver enqueues item, folding in any gap accumulated since the last delivery.
func (s *subscriber[T]) deliver(item Seq[T]) {
	// Detached between Publish's snapshot and now.
	select {
	case <-s.done:
		return
	default:
	}

	s.mu.Lock()
	d := delivery[T]{item: item}
	if s.dropped > 0 {
		d.gap = &GapError{Dropped: s.dropped, LastGood: s.lastGood, Next: item.Seq}
	}
	s.mu.Unlock()

	select {
	case s.ch <- d:
		s.mu.Lock()
		s.dropped = 0
		s.lastGood = item.Seq
		s.mu.Unlock()
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// Subscribe attaches a subscriber and returns its stream plus an idempotent
// detach function that must be called (ranging to completion does not detach).
// fromSeq replays retained items above it first; a non-nil error is always a
// *GapError — see spec §2.11.
func (f *Fanout[T]) Subscribe(fromSeq int) (iter.Seq2[Seq[T], error], func()) {
	s := &subscriber[T]{
		ch:       make(chan delivery[T], f.opts.Subscriber),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		lastGood: fromSeq,
	}

	// Registration and backlog delivery are one step under pubMu — see spec §2.11.
	f.pubMu.Lock()
	defer f.pubMu.Unlock()

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return emptyStream[T], func() {}
	}
	f.nextID++
	s.id = f.nextID
	var backlog []Seq[T]
	for _, item := range f.replay {
		if item.Seq > fromSeq {
			backlog = append(backlog, item)
		}
	}
	// A cursor below the replay window is a gap running forward — see spec §2.11.
	if fromSeq >= 0 && fromSeq < f.seq {
		first := f.seq + 1
		if len(f.replay) > 0 {
			first = f.replay[0].Seq
		}
		s.dropped = first - 1 - fromSeq
	}
	reset, resumeAt := false, 0
	if fromSeq > f.seq {
		// Ahead of the head: a timeline reset — see spec §2.11.
		s.lastGood = 0
		s.dropped = fromSeq
		resumeAt = f.seq + 1
		reset = true
	}
	f.subs[s.id] = s
	f.mu.Unlock()

	for _, item := range backlog {
		s.deliver(item)
	}

	cancel := sync.OnceFunc(func() {
		f.mu.Lock()
		delete(f.subs, s.id)
		f.mu.Unlock()
		close(s.done)
	})

	// emitFinalGap reports drops that never got a later delivery to ride out on.
	emitFinalGap := func(yield func(Seq[T], error) bool) {
		s.mu.Lock()
		n, last := s.dropped, s.lastGood
		s.dropped = 0
		s.mu.Unlock()
		if n > 0 {
			yield(Seq[T]{}, &GapError{Dropped: n, LastGood: last})
		}
	}

	emit := func(yield func(Seq[T], error) bool, d delivery[T]) bool {
		if d.gap != nil {
			return yield(d.item, d.gap)
		}
		return yield(d.item, nil)
	}

	stream := func(yield func(Seq[T], error) bool) {
		// A timeline reset is reported immediately, with Next set (not AtEnd).
		if reset {
			s.mu.Lock()
			n, last := s.dropped, s.lastGood
			s.dropped = 0
			s.mu.Unlock()
			if n > 0 && !yield(Seq[T]{}, &GapError{Dropped: n, LastGood: last, Next: resumeAt}) {
				return
			}
		}
		for {
			select {
			case <-s.done:
				return
			case d := <-s.ch:
				if !emit(yield, d) {
					return
				}
			case <-s.finished:
				// Close: drain the buffer, report any final gap, end.
				for {
					select {
					case d := <-s.ch:
						if !emit(yield, d) {
							return
						}
					default:
						emitFinalGap(yield)
						return
					}
				}
			}
		}
	}
	return stream, cancel
}

// Close ends every subscriber's stream once its buffered items are delivered.
// Idempotent.
func (f *Fanout[T]) Close() {
	// pubMu first: an accepted publish lands before the streams end.
	f.pubMu.Lock()
	defer f.pubMu.Unlock()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	for id, s := range f.subs {
		close(s.finished)
		delete(f.subs, id)
	}
}

// LastSeq reports the sequence number of the most recently published item (zero
// if none).
func (f *Fanout[T]) LastSeq() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq
}

func emptyStream[T any](func(Seq[T], error) bool) {}
