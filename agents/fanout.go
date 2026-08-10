package agents

import (
	"fmt"
	"iter"
	"sync"
)

// Seq pairs a broadcast value with its position in the stream. The number is
// assigned at publish time and is monotonic per Fanout, which is what makes
// both replay (resume after N) and gap detection (the next number is not N+1)
// possible.
type Seq[T any] struct {
	Seq   int
	Value T
}

// GapError reports that a subscriber fell behind and items were dropped for it.
// It is delivered in-band, on that subscriber's stream only, immediately before
// the next item that did get through.
//
// A consumer that receives one has an incomplete view and must recover — by
// re-subscribing from LastGood, or by re-reading whatever durable record the
// producer keeps. Rendering onward without recovering shows a timeline that is
// silently missing content.
type GapError struct {
	// Dropped is how many items were discarded.
	Dropped int
	// LastGood is the sequence number of the last item that was delivered
	// before the gap; Resume from it.
	LastGood int
	// Next is the sequence number of the item delivered right after the gap,
	// or 0 when the gap runs to the end of the stream — the producer finished
	// while this subscriber was still behind, so there is no "after". The item
	// yielded alongside such a gap is the zero value and carries nothing.
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

// Buffer sizes chosen to be generous rather than tuned: dropping is a
// correctness event that costs a subscriber a resync, so the buffer should
// absorb an ordinary slow render, and only a genuinely stuck consumer should
// overflow it.
const (
	// DefaultSubscriberBuffer is the per-subscriber buffer when unset.
	DefaultSubscriberBuffer = 256
)

// Fanout broadcasts one producer's items to many independent subscribers,
// decoupling the producer from all of them: a run's events go to every
// attached client, and one slow client must not stall the run.
//
// The slow-subscriber policy: Publish never blocks. When a subscriber's buffer
// is full its items are dropped LOUDLY — the next delivery on that stream is
// preceded by a *GapError naming the range it lost, so the consumer can resync
// from LastGood. Why loud dropping beats silent dropping and disconnection,
// and why per-subscriber buffering is required for any stream shape, is spec
// §2.11.
//
// A Fanout is safe for concurrent use. Subscribers may come and go at any time.
type Fanout[T any] struct {
	opts FanoutOptions

	// pubMu serializes a publish end to end — sequence assignment through
	// delivery — so two concurrent publishers can never make a subscriber
	// observe a higher sequence number before a lower one. Assigning under mu
	// and delivering outside it is not enough: the goroutine that took seq 2
	// can reach a subscriber before the one that took seq 1.
	//
	// Lock order is pubMu before mu, and mu is never held during delivery, so
	// Subscribe and Close stay responsive while events flow. Deliveries are
	// non-blocking sends, so holding pubMu across them cannot stall anything.
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
	// done closes when this subscriber detaches; finished closes when the
	// producer is done. They are separate channels, never the data channel:
	// closing ch would race a concurrent Publish into a send-on-closed panic,
	// since Publish delivers outside the Fanout lock by design.
	done     chan struct{}
	finished chan struct{}

	// mu guards the drop bookkeeping, which the publisher writes and the
	// delivery path reads.
	mu       sync.Mutex
	dropped  int
	lastGood int
}

// delivery carries an item plus the gap (if any) that immediately precedes it.
// Attaching the gap to the next successful send is what makes reporting
// possible at all: when the buffer is full there is by definition no room to
// enqueue a separate gap notice.
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
// It never blocks: a subscriber that cannot keep up loses items rather than
// holding up the producer or its peers.
//
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
	// A subscriber that detached between Publish's snapshot and now needs
	// nothing more; dropping into its buffer would be harmless but pointless.
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

// Subscribe attaches a new subscriber and returns its stream plus a function
// that detaches it. The stream yields each item in sequence order; a non-nil
// error is always a *GapError.
//
// The item alongside a gap is the first one after it and is still valid —
// EXCEPT for a gap that runs to the end of the stream (GapError.AtEnd), which
// has no "after". There the item is the zero value: the producer finished
// while this subscriber was behind, so the loss is reported as the stream
// closes rather than never.
//
// fromSeq replays retained items with a higher sequence number before live
// delivery begins, so a reconnecting consumer resumes without a hole. Pass 0
// for everything retained. Replay respects the subscriber buffer: a consumer
// that asks for more history than it will read gets a gap like any other.
//
// The returned cancel function is idempotent and must be called, or the
// subscriber's buffer is retained until the Fanout is garbage-collected.
// Ranging to completion does not detach — a consumer may stop early.
func (f *Fanout[T]) Subscribe(fromSeq int) (iter.Seq2[Seq[T], error], func()) {
	s := &subscriber[T]{
		ch:       make(chan delivery[T], f.opts.Subscriber),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		lastGood: fromSeq,
	}

	// Hold pubMu across registration AND backlog delivery. Registering first
	// and replaying after would let a concurrent Publish reach the subscriber
	// ahead of its own backlog, delivering seq 9 before seq 5. Same lock order
	// as Publish (pubMu, then mu), so this cannot deadlock.
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
	// A cursor from OUTSIDE the stream's reachable range is a gap, and "a
	// dropped item is always reported" applies to it exactly as to a slow
	// consumer. Older than the replay window: the range up to the window's
	// start was evicted and can never be delivered; the gap runs forward from
	// the cursor. Ahead of the head: this fanout did not issue that cursor (a
	// restart recreated the stream and its numbering), so the gap is a
	// TIMELINE RESET — LastGood drops to 0 and Dropped carries the cursor, so
	// the consumer's documented recovery (resubscribe from LastGood) replays
	// the new timeline from its start instead of chasing a number this stream
	// will not reach for a long time, and the gap's own sequence never runs
	// backwards past the deliveries that follow it.
	if len(f.replay) > 0 && fromSeq >= 0 && fromSeq < f.replay[0].Seq-1 {
		s.dropped = f.replay[0].Seq - 1 - fromSeq
	}
	reset, resumeAt := false, 0
	if fromSeq > f.seq {
		s.lastGood = 0
		s.dropped = fromSeq
		// Where the consumer picks the new timeline up: the next sequence this
		// fanout will issue.
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

	// emitFinalGap reports drops that never got a later delivery to ride out
	// on. Without it the last stretch of loss is silent, and a consumer cannot
	// tell a timeline missing its tail from one that ended there — which is
	// the whole reason drops are reported at all.
	emitFinalGap := func(yield func(Seq[T], error) bool, s *subscriber[T]) {
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
		// A timeline reset is reported IMMEDIATELY rather than riding out on
		// the next delivery: the stream a stale cursor lands on has often
		// already ended (a finished run, replayed after a restart), so there
		// is no next delivery and the consumer would sit in silence until the
		// producer closed — minutes later, or never. It is the one gap known
		// at subscribe time, so it is the one gap that can be told at once.
		if reset {
			s.mu.Lock()
			n, last := s.dropped, s.lastGood
			s.dropped = 0
			s.mu.Unlock()
			// Next is the sequence the stream WILL resume at, not zero: zero
			// means AtEnd — "nothing further will arrive to close this gap" —
			// and announcing that on a stream about to deliver tells a
			// consumer following the documented meaning to stop reading a live
			// run. A reset points at the next sequence this timeline issues.
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
				// The producer is done. Deliver what is already buffered —
				// Close means "no more will be published", not "discard what
				// you have" — then report anything still dropped and end.
				for {
					select {
					case d := <-s.ch:
						if !emit(yield, d) {
							return
						}
					default:
						emitFinalGap(yield, s)
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
	// pubMu before mu, the same order Publish and Subscribe take. Closing
	// under mu alone would let Close slip into the window where Publish has
	// released mu and not yet delivered: the item has its sequence number and
	// is in replay, but every stream has already ended, so it is lost with no
	// gap to report it. Taking pubMu makes Close wait for an accepted publish
	// to land, which is what "items already buffered are still delivered"
	// means.
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
