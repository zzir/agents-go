package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// A gap that runs to the end of the stream carries the ZERO item — for
// *protocol.Envelope that is a nil pointer. Forwarding it to the sink hands
// every consumer a nil envelope, and the SSE sink dereferences Env.Type on the
// subscriber's own goroutine, so the panic takes the process down rather than
// the request.
func TestSubscribeSeqNeverDeliversNilEnvelope(t *testing.T) {
	// The broadcaster closes on shutdown or GC, which is when a gap that never
	// got a later delivery is finally reported.
	rootCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	h := NewRunHub(rootCtx)
	if _, _, err := h.register("run1", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The sink blocks until released, so the subscriber goroutine cannot drain
	// its buffer and the publisher drops onto the floor — which is what leaves
	// a gap outstanding when the run ends.
	release := make(chan struct{})
	var mu sync.Mutex
	var got []SeqEnvelope
	blocked := make(chan struct{}, 1)
	sink := func(item SeqEnvelope) {
		select {
		case blocked <- struct{}{}:
			<-release
		default:
		}
		mu.Lock()
		got = append(got, item)
		mu.Unlock()
	}

	if _, ok := h.SubscribeSeq("run1", 0, sink); !ok {
		t.Fatal("subscribe failed")
	}

	// Fill the subscriber buffer and then overflow it well past capacity.
	h.publish("run1", env("run.started"))
	<-blocked // the sink is now parked holding the first event
	for range EventBufferCap * 2 {
		h.publish("run1", env("run.step"))
	}

	// The producer finishes while this subscriber is still behind: the gap has
	// no following item to ride out on.
	h.finish("run1", false)
	shutdown()
	close(release)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= EventBufferCap {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d events drained", n)
		case <-time.After(time.Millisecond):
		}
	}
	// Let the trailing gap arrive.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	sawGap := false
	for i, item := range got {
		if item.Env == nil {
			t.Fatalf("delivery %d of %d has a nil envelope", i, len(got))
		}
		if item.Env.Type == protocol.EventRunGap {
			sawGap = true
		}
	}
	if !sawGap {
		t.Fatal("a subscriber that lost the tail of the stream was never told")
	}
}
