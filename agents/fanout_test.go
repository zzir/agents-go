package agents

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// drainFanout reads a stream until it ends, recording values and gaps.
func drainFanout(stream func(func(Seq[int], error) bool)) ([]int, []*GapError) {
	var got []int
	var gaps []*GapError
	for item, err := range stream {
		var gap *GapError
		if errors.As(err, &gap) {
			gaps = append(gaps, gap)
		}
		got = append(got, item.Value)
	}
	return got, gaps
}

// The whole point: a subscriber that never reads must not hold up the producer.
// This is the property both a plain iterator and a plain buffered channel fail.
func TestFanoutPublishNeverBlocks(t *testing.T) {
	f := NewFanout[int](FanoutOptions{Subscriber: 4})
	_, cancel := f.Subscribe(0) // attached and never read
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := range 10_000 {
			f.Publish(i)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads")
	}
	if f.LastSeq() != 10_000 {
		t.Errorf("LastSeq = %d, want 10000", f.LastSeq())
	}
}

// A slow subscriber must never lose items silently. Silent loss is the failure
// mode this design exists to prevent: a timeline missing a tool result looks
// exactly like one that never had it.
func TestFanoutReportsGaps(t *testing.T) {
	f := NewFanout[int](FanoutOptions{Subscriber: 2})
	stream, cancel := f.Subscribe(0)
	defer cancel()

	for i := 1; i <= 10; i++ {
		f.Publish(i) // 1,2 buffer; 3..9 drop; 10 drops too
	}
	f.Close()

	got, gaps := drainFanout(stream)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("delivered %v, want the two that fit", got)
	}
	if len(gaps) != 0 {
		t.Fatalf("no gap can be reported yet — nothing got through after the drops; got %v", gaps)
	}

	// The gap surfaces on the next successful delivery, so verify that path
	// with a subscriber that drains between publishes.
	f2 := NewFanout[int](FanoutOptions{Subscriber: 2})
	stream2, cancel2 := f2.Subscribe(0)
	defer cancel2()

	for i := 1; i <= 6; i++ {
		f2.Publish(i) // 1,2 buffer; 3,4,5,6 drop
	}
	// Drain so there is room again, then publish one more.
	next, stop := iterPull(stream2)
	a, _, _ := next()
	b, _, _ := next()
	if a.Value != 1 || b.Value != 2 {
		t.Fatalf("first two = %d,%d, want 1,2", a.Value, b.Value)
	}
	f2.Publish(7)
	c, cErr, _ := next()
	stop()

	if c.Value != 7 {
		t.Errorf("item after the gap = %d, want 7", c.Value)
	}
	var gap *GapError
	if !errors.As(cErr, &gap) {
		t.Fatalf("no gap reported after 4 drops; err = %v", cErr)
	}
	if gap.Dropped != 4 {
		t.Errorf("gap.Dropped = %d, want 4", gap.Dropped)
	}
	if gap.LastGood != 2 {
		t.Errorf("gap.LastGood = %d, want 2 (resume point)", gap.LastGood)
	}
	if gap.Next != 7 {
		t.Errorf("gap.Next = %d, want 7", gap.Next)
	}
}

// One slow subscriber must not cost a fast one anything. Without per-subscriber
// buffers this is exactly what breaks.
//
// The test is a lock-step handshake rather than a race: publish one item, read
// it on the fast stream, repeat. That way the fast subscriber provably never
// overflows regardless of buffer size or scheduling, so any gap it sees is the
// slow peer's fault — which is the claim under test.
func TestFanoutIsolatesSubscribers(t *testing.T) {
	const buffer = 4
	f := NewFanout[int](FanoutOptions{Subscriber: buffer})

	_, cancelSlow := f.Subscribe(0) // attached, never read
	defer cancelSlow()
	fast, cancelFast := f.Subscribe(0)
	defer cancelFast()

	next, stop := iterPull(fast)
	defer stop()

	const n = 500
	for i := 1; i <= n; i++ {
		f.Publish(i)
		item, err, ok := next()
		if !ok {
			t.Fatalf("fast stream ended at item %d", i)
		}
		if err != nil {
			t.Fatalf("fast subscriber saw a gap at item %d (%v); a slow peer must not cost it anything", i, err)
		}
		if item.Value != i {
			t.Fatalf("fast subscriber got %d, want %d — order broke", item.Value, i)
		}
		if item.Seq != i {
			t.Fatalf("seq = %d for item %d; sequence must be monotonic and hole-free here", item.Seq, i)
		}
	}

	// And the slow one really did overflow, or the test proved nothing.
	if f.LastSeq() != n {
		t.Errorf("LastSeq = %d, want %d", f.LastSeq(), n)
	}
}

// A reconnecting consumer resumes from its last good sequence number without a
// hole, which is what makes dropping recoverable rather than fatal.
func TestFanoutReplay(t *testing.T) {
	f := NewFanout[int](FanoutOptions{Replay: 10})
	for i := 1; i <= 8; i++ {
		f.Publish(i)
	}

	stream, cancel := f.Subscribe(5) // resume after seq 5
	defer cancel()
	f.Close()

	got, gaps := drainFanout(stream)
	if len(gaps) != 0 {
		t.Errorf("replay reported gaps: %v", gaps)
	}
	want := []int{6, 7, 8}
	if len(got) != len(want) {
		t.Fatalf("replayed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replayed %v, want %v", got, want)
		}
	}

	// Replay is bounded: an item older than the ring is gone, and asking for
	// everything yields only what was retained.
	f2 := NewFanout[int](FanoutOptions{Replay: 3})
	for i := 1; i <= 10; i++ {
		f2.Publish(i)
	}
	s2, c2 := f2.Subscribe(0)
	defer c2()
	f2.Close()
	got2, _ := drainFanout(s2)
	if len(got2) != 3 || got2[0] != 8 {
		t.Errorf("bounded replay = %v, want the last 3 (8,9,10)", got2)
	}

	// Replay off by default.
	f3 := NewFanout[int](FanoutOptions{})
	f3.Publish(1)
	s3, c3 := f3.Subscribe(0)
	defer c3()
	f3.Close()
	if got3, _ := drainFanout(s3); len(got3) != 0 {
		t.Errorf("replay should be off by default, got %v", got3)
	}
}

// Close means "no more will be published", not "discard what you have".
func TestFanoutCloseDrainsBuffered(t *testing.T) {
	f := NewFanout[int](FanoutOptions{Subscriber: 16})
	stream, cancel := f.Subscribe(0)
	defer cancel()

	for i := 1; i <= 5; i++ {
		f.Publish(i)
	}
	f.Close()
	f.Close()      // idempotent
	f.Publish(999) // no-op after close

	got, _ := drainFanout(stream)
	if len(got) != 5 {
		t.Fatalf("got %v, want the 5 buffered items", got)
	}
	if got[4] == 999 {
		t.Error("Publish after Close was not a no-op")
	}

	// Subscribing to a closed Fanout yields an immediately-empty stream.
	late, lateCancel := f.Subscribe(0)
	defer lateCancel()
	if g, _ := drainFanout(late); len(g) != 0 {
		t.Errorf("late subscriber to a closed fanout got %v", g)
	}
}

func TestFanoutCancel(t *testing.T) {
	f := NewFanout[int](FanoutOptions{Subscriber: 8})
	stream, cancel := f.Subscribe(0)

	f.Publish(1)
	if f.Subscribers() != 1 {
		t.Fatalf("Subscribers = %d, want 1", f.Subscribers())
	}
	cancel()
	cancel() // idempotent — must not panic
	if f.Subscribers() != 0 {
		t.Errorf("Subscribers = %d after cancel, want 0", f.Subscribers())
	}

	// The cancelled stream ends rather than hanging.
	done := make(chan struct{})
	go func() { drainFanout(stream); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled stream did not terminate")
	}

	// Publishing to a fanout whose only subscriber cancelled is fine.
	f.Publish(2)
}

// Cancelling while Publish is mid-fan-out must not panic. Publish snapshots
// subscribers and delivers outside the lock, so this window is real.
func TestFanoutCancelDuringPublishIsSafe(t *testing.T) {
	f := NewFanout[int](FanoutOptions{Subscriber: 1})
	defer f.Close()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				stream, cancel := f.Subscribe(0)
				go func() { drainFanout(stream) }()
				cancel()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 2000 {
			f.Publish(i)
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent subscribe/cancel/publish deadlocked")
	}
}

// OnDrop is observation only; it must see every drop the subscriber is later
// told about.
func TestFanoutOnDrop(t *testing.T) {
	var drops atomic.Int64
	f := NewFanout[int](FanoutOptions{
		Subscriber: 2,
		OnDrop:     func(int, int) { drops.Add(1) },
	})
	_, cancel := f.Subscribe(0)
	defer cancel()

	for i := 1; i <= 10; i++ {
		f.Publish(i)
	}
	if got := drops.Load(); got != 8 {
		t.Errorf("OnDrop fired %d times, want 8 (10 published, 2 buffered)", got)
	}
}

// iterPull adapts a range-over-func stream to a pull interface, so a test can
// interleave reads with publishes.
func iterPull(stream func(func(Seq[int], error) bool)) (func() (Seq[int], error, bool), func()) {
	type result struct {
		item Seq[int]
		err  error
	}
	out := make(chan result)
	stop := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(out)
		for item, err := range stream {
			select {
			case out <- result{item, err}:
			case <-stop:
				return
			}
		}
	}()

	next := func() (Seq[int], error, bool) {
		select {
		case r, ok := <-out:
			return r.item, r.err, ok
		case <-time.After(5 * time.Second):
			return Seq[int]{}, nil, false
		}
	}
	return next, func() { once.Do(func() { close(stop) }) }
}

// Concurrent publishers must not let a subscriber observe a higher sequence
// number before a lower one. Assigning the number under one lock and delivering
// after releasing it is not enough — the goroutine holding seq 2 can reach the
// subscriber first.
func TestFanoutOrdersConcurrentPublishes(t *testing.T) {
	const publishers, each = 8, 300
	f := NewFanout[int](FanoutOptions{Subscriber: publishers * each})
	stream, cancel := f.Subscribe(0)
	defer cancel()

	var wg sync.WaitGroup
	for p := range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				f.Publish(p*each + i)
			}
		}()
	}
	wg.Wait()
	f.Close()

	last := 0
	n := 0
	for item, err := range stream {
		if err != nil {
			t.Fatalf("unexpected gap with a buffer sized for everything: %v", err)
		}
		if item.Seq <= last {
			t.Fatalf("sequence went backwards: %d after %d", item.Seq, last)
		}
		last = item.Seq
		n++
	}
	if n != publishers*each {
		t.Errorf("received %d items, want %d", n, publishers*each)
	}
}

// A subscriber's backlog must arrive before anything published after it
// attached. Registering first and replaying after would let a concurrent
// publish jump the queue and deliver a later sequence number first.
func TestFanoutReplayOrdersAgainstConcurrentPublish(t *testing.T) {
	for range 50 {
		f := NewFanout[int](FanoutOptions{Replay: 64, Subscriber: 256})
		for i := 1; i <= 32; i++ {
			f.Publish(i)
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 33; i <= 64; i++ {
				f.Publish(i)
			}
		}()

		stream, cancel := f.Subscribe(0)
		wg.Wait()
		f.Close()

		last := 0
		for item, err := range stream {
			if err != nil {
				continue // a gap is fine here; order is what is under test
			}
			if item.Seq <= last {
				t.Fatalf("backlog and live delivery interleaved: seq %d after %d", item.Seq, last)
			}
			last = item.Seq
		}
		cancel()
	}
}
