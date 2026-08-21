package agents

import (
	"errors"
	"sync"
	"testing"
)

// Close must not overtake a Publish that has already been accepted. Publish
// assigns the sequence number and snapshots the subscriber list under mu, then
// delivers outside it; a Close that slips into that window ends every stream
// before the delivery lands. The item then has a sequence number, sits in
// replay, and is in the subscriber's buffer — but its stream already returned,
// so it is lost with no gap to report it. Silent loss is the one thing Fanout
// promises never to do.
func TestCloseDoesNotOvertakeAnAcceptedPublish(t *testing.T) {
	for range 3000 {
		f := NewFanout[int](FanoutOptions{Subscriber: 8, Replay: 8})
		stream, cancel := f.Subscribe(0)

		var (
			mu   sync.Mutex
			seen []int
			gaps int
		)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for item, err := range stream {
				if gap, ok := errors.AsType[*GapError](err); ok {
					mu.Lock()
					gaps++
					mu.Unlock()
					if gap.AtEnd() {
						continue
					}
				}
				mu.Lock()
				seen = append(seen, item.Value)
				mu.Unlock()
			}
		}()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); f.Publish(42) }()
		go func() { defer wg.Done(); f.Close() }()
		wg.Wait()
		<-done
		cancel()

		// LastSeq is non-zero exactly when Publish got in before Close. Having
		// been accepted, it must reach the subscriber — or be reported missing.
		mu.Lock()
		delivered := len(seen) > 0
		reported := gaps > 0
		mu.Unlock()
		if f.LastSeq() != 0 && !delivered && !reported {
			t.Fatalf("published item seq %d was neither delivered nor reported as a gap", f.LastSeq())
		}
	}
}
