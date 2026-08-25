package handler

import (
	"sync"
	"testing"
)

// AttachAll subscribes from a snapshot of the registry taken under its lock and
// released before it subscribes, so a socket that closes in that window is
// still in the snapshot. The subscription it then records would have nobody
// left to detach it: closeAll already ran. It fed a dead connection — and
// called Close on it once per event — until the run's fanout closed, minutes
// later.
func TestConnSubs_SubscribeAfterCloseDetachesItself(t *testing.T) {
	cs := &connSubs{subs: map[string]func(){}}
	cs.closeAll()

	detached := false
	cs.add("run-1", func() { detached = true })

	if !detached {
		t.Fatal("a subscription recorded after closeAll was never detached")
	}
	if cs.has("run-1") {
		t.Fatal("a closed set must not hold subscriptions")
	}
}

// The ordinary path is unchanged: re-subscribing to the same run detaches the
// previous sink, or every event is delivered twice.
func TestConnSubs_ResubscribeDetachesThePrevious(t *testing.T) {
	cs := &connSubs{subs: map[string]func(){}}
	first := false
	cs.add("run-1", func() { first = true })
	cs.add("run-1", func() {})
	if !first {
		t.Fatal("re-subscribing left the previous sink attached")
	}

	second := false
	cs.subs["run-2"] = func() { second = true }
	cs.closeAll()
	if !second {
		t.Fatal("closeAll left a subscription attached")
	}
}

// closeAll races the AttachAll it is meant to shut out; every subscription must
// end up detached exactly once regardless of who wins.
func TestConnSubs_CloseRacesAdd(t *testing.T) {
	cs := &connSubs{subs: map[string]func(){}}

	var mu sync.Mutex
	detached := 0
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 50 {
			_ = i
			cs.add("run-1", func() { mu.Lock(); detached++; mu.Unlock() })
		}
	}()
	go func() {
		defer wg.Done()
		cs.closeAll()
	}()
	wg.Wait()
	cs.closeAll()

	mu.Lock()
	defer mu.Unlock()
	if detached != 50 {
		t.Fatalf("50 subscriptions, %d detached", detached)
	}
}
