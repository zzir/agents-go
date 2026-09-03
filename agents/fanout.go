package agents

import (
	"fmt"
	"iter"
	"sync"
)

// Seq pairs a broadcast value with its position in the stream: monotonic per
// Fanout, assigned at publish, so replay and gap detection are both possible.
type Seq[T any] struct {
	Seq   int
	Value T
}

// GapError reports that a subscriber fell behind and items were dropped for
// it, delivered in-band on that subscriber's stream before the next item that
// got through (spec §2.11). A consumer that receives one must recover: re-
// subscribe from LastGood, or re-read the producer's durable record.
type GapError struct {
	// Dropped is how many items were discarded.
	Dropped int
	// LastGood is the sequence number of the last item that was delivered
	// before the gap; Resume from it.
	LastGood int
	// Next is the sequence number of the item delivered right after the gap,
	// or 0 when the gap runs to the end of the stream — the item yielded
	// alongside such a gap is the zero value and carries nothing.
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

// AtEnd reports whether the gap runs to the end of the stream, meaning nothing
// further will arrive to close it. A consumer that resyncs on a gap has no
// "next" item to anchor on here and must resume from LastGood.
func (e *GapError) AtEnd() bool { return e.Next == 0 }

// FanoutOptions configures a Fanout. The zero value is usable.
type FanoutOptions struct {
	// Subscriber is the per-subscriber buffer, in items. A subscriber that
	// falls this far behind starts dropping — on its own stream only.
	// Defaults to DefaultSubscriberBuffer.
	Subscriber int

	// Replay is how many recent items to retain for subscribers that attach
	// late or reattach after a disconnect. Zero disables replay: such a
	// subscriber sees only what is published from then on.
	Replay int
}

// Buffer sizes are generous rather than tuned: a drop costs a subscriber a
// resync, so only a genuinely stuck consumer should overflow.
const (
	// DefaultSubscriberBuffer is the per-subscriber buffer when unset.
	DefaultSubscriberBuffer = 256
)

// Fanout broadcasts one producer's items to many independent subscribers:
// Publish never blocks, and a subscriber whose buffer is full loses items
// LOUDLY — a *GapError names the range it lost (spec §2.11). Safe for
// concurrent use; subscribers may come and go at any time.
type Fanout[T any] struct {
	opts FanoutOptions

	// pubMu serializes a publish end to end, so no subscriber sees seq 2
	// before seq 1. Lock order: pubMu before mu; mu is never held in delivery.
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
	// done closes when this subscriber detaches; finished when the producer
	// is done. Closing ch itself would race a concurrent Publish.
	done     chan struct{}
	finished chan struct{}

	// mu guards the drop bookkeeping, which the publisher writes and the
	// delivery path reads.
	mu       sync.Mutex
	dropped  int
	lastGood int
}

// delivery carries an item plus the gap (if any) that immediately precedes it:
// a full buffer has no room for a separate gap notice.
type delivery[T any] struct {
	item Seq[T]
	gap  *GapError
}

// NewFanout creates a Fanout. Call Close when the producer is done so
// subscribers' iterators terminate.
func NewFanout[T any](opts FanoutOptions) *Fanout[T] {
	if opts.Subscriber <= 0 {
		opts.Subscriber = DefaultSubscriberBuffer
	}
	return &Fanout[T]{opts: opts, subs: make(map[int]*subscriber[T])}
}

// Publish assigns the next sequence number and delivers to every subscriber.
// It never blocks: a subscriber that cannot keep up loses items instead.
// Publishing after Close is a no-op.
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

	// Delivery happens outside mu (but still under pubMu) so Subscribe and
	// Close stay responsive while events flow.
	for _, s := range subs {
		s.deliver(item)
	}
}

// deliver enqueues item, folding in any gap accumulated since this subscriber's
// last successful delivery.
func (s *subscriber[T]) deliver(item Seq[T]) {
	// Detached between Publish's snapshot and now: nothing more to deliver.
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

// Subscribe attaches a subscriber and returns its stream plus a function that
// detaches it. Items arrive in sequence order; a non-nil error is always a
// *GapError, beside the first item after the gap — except a gap running to
// the end of the stream (GapError.AtEnd), whose item is the zero value.
// fromSeq replays retained items with a higher sequence number first (0 for
// everything retained); replay respects the subscriber buffer. The cancel
// function is idempotent and must be called, or the subscriber's buffer is
// retained until the Fanout is collected; ranging to completion does not detach.
func (f *Fanout[T]) Subscribe(fromSeq int) (iter.Seq2[Seq[T], error], func()) {
	s := &subscriber[T]{
		ch:       make(chan delivery[T], f.opts.Subscriber),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		lastGood: fromSeq,
	}

	// Registration and backlog delivery are one step under pubMu, or a
	// concurrent Publish reaches the subscriber ahead of its own backlog.
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
	// A cursor behind the reachable range is a gap like any drop: what it
	// missed can never be delivered, so the gap runs forward (spec §2.11).
	if fromSeq >= 0 && fromSeq < f.seq {
		first := f.seq + 1
		if len(f.replay) > 0 {
			first = f.replay[0].Seq
		}
		s.dropped = first - 1 - fromSeq
	}
	reset, resumeAt := false, 0
	if fromSeq > f.seq {
		// Ahead of the head: a cursor from a previous life of the stream is a
		// timeline reset (spec §2.11) — LastGood 0, replay from resumeAt.
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

	// emitFinalGap reports drops that never got a later delivery to ride out on,
	// so a consumer can tell a timeline missing its tail from one that ended there.
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
		// A timeline reset is reported immediately: the stream a stale cursor
		// lands on has often already ended, with no next delivery to carry it.
		if reset {
			s.mu.Lock()
			n, last := s.dropped, s.lastGood
			s.dropped = 0
			s.mu.Unlock()
			// Next is where the stream resumes, not zero (AtEnd), or a consumer
			// would stop reading a live run.
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
				// Close means "no more will be published", not "discard what
				// you have": drain the buffer, report any final gap, end.
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

// Close ends every subscriber's stream. Items already buffered for a subscriber
// are still delivered — closing means "no more will be published", not "discard
// what you have". Close is idempotent.
func (f *Fanout[T]) Close() {
	// pubMu first, so an accepted publish lands before the streams end (spec
	// §2.11); same lock order as Publish and Subscribe.
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

// LastSeq reports the sequence number of the most recently published item, or
// zero if nothing has been published. A consumer stores it to resume later.
func (f *Fanout[T]) LastSeq() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq
}

func emptyStream[T any](func(Seq[T], error) bool) {}
