package bridge

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// A subscriber that attaches from seq 0 after the replay ring has moved on — a
// browser reloaded a minute into a run that streams a delta per token — must
// still be told WHICH run this is: run.started is the only event that maps a
// run id to its session, and a client that never saw it drops every other
// event of the run on the floor.
func TestSubscribeFromZeroDeliversRunStartedAfterRingMovedOn(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	started, err := protocol.NewEnvelope(protocol.EventRunStarted, protocol.RunStarted{RunID: "run1", SessionID: "sess1", Input: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	h.publish("run1", started)
	// Well past the ring: seq 1 is long evicted by the time anyone attaches.
	for range EventBufferCap + 50 {
		h.publish("run1", env(protocol.EventRunStep))
	}

	var mu sync.Mutex
	var got []SeqEnvelope
	if _, ok := h.SubscribeSeq("run1", 0, func(item SeqEnvelope) {
		mu.Lock()
		got = append(got, item)
		mu.Unlock()
	}); !ok {
		t.Fatal("subscribe failed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= EventBufferCap {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d events replayed", n)
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if got[0].Env.Type != protocol.EventRunStarted {
		types := make([]string, 0, 3)
		for _, item := range got[:3] {
			types = append(types, item.Env.Type)
		}
		t.Fatalf("a late subscriber's stream opens with %v; run.started must come first, or the client cannot place a single event", types)
	}
	var sp protocol.RunStarted
	if err := json.Unmarshal(got[0].Env.Payload, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.SessionID != "sess1" || sp.Input != "hello" {
		t.Fatalf("run.started = %+v, want the one the run announced", sp)
	}
	if got[0].Seq != 1 {
		t.Fatalf("pinned run.started carries seq %d, want its own (1)", got[0].Seq)
	}
	// Told once: the identity is not repeated at the end of the replay, and
	// the ring's tail follows in order.
	for i, item := range got[1:] {
		if item.Env.Type == protocol.EventRunStarted {
			t.Fatalf("run.started again at position %d", i+1)
		}
	}
}

// While the ring still holds run.started, the replay is the ring's alone: the
// pin must not put a second run.started in front of the one the ring delivers,
// and a cursor already past the start (a resubscribe) is not told again.
func TestSubscribeDoesNotRepeatRunStartedTheRingStillHolds(t *testing.T) {
	h := NewRunHub(context.Background())
	if _, _, err := h.register("run1", "sess1", "", "", "", "", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	h.publish("run1", env(protocol.EventRunStarted))
	for range 10 {
		h.publish("run1", env(protocol.EventRunStep))
	}
	collect := func(fromSeq int) []SeqEnvelope {
		var mu sync.Mutex
		var got []SeqEnvelope
		if _, ok := h.SubscribeSeq("run1", fromSeq, func(item SeqEnvelope) {
			mu.Lock()
			got = append(got, item)
			mu.Unlock()
		}); !ok {
			t.Fatal("subscribe failed")
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			n := len(got)
			mu.Unlock()
			if n >= 11-fromSeq {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("from %d: only %d events replayed", fromSeq, n)
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // anything extra would have landed by now
		mu.Lock()
		defer mu.Unlock()
		return append([]SeqEnvelope(nil), got...)
	}
	fromZero := collect(0)
	if n := countType(fromZero, protocol.EventRunStarted); n != 1 || fromZero[0].Env.Type != protocol.EventRunStarted || fromZero[0].Seq != 1 {
		t.Fatalf("from 0: %d run.started, first %s@%d — want exactly the ring's own, first", n, fromZero[0].Env.Type, fromZero[0].Seq)
	}
	if fromFive := collect(5); countType(fromFive, protocol.EventRunStarted) != 0 {
		t.Fatalf("a cursor past the start was told run.started again: %v", fromFive)
	}
}

func countType(items []SeqEnvelope, typ string) int {
	n := 0
	for _, item := range items {
		if item.Env != nil && item.Env.Type == typ {
			n++
		}
	}
	return n
}
