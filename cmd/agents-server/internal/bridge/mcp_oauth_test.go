package bridge

import (
	"context"
	"testing"
	"time"
)

// A stale interactive OAuth attempt (the user refreshed mid-flow) must be
// cancelled and awaited when a fresh authorize arrives, so the connect slot is
// free again instead of "already in progress" until the 5-minute timeout.
func TestOAuthCoordinatorSupersedeInflight(t *testing.T) {
	c := NewOAuthCoordinator(nil)
	const id = "srv-1"

	// Register a stale attempt whose goroutine releases (closes done) on cancel,
	// mirroring how ConnectWithOAuth's goroutine unwinds ConnectHTTPWithOAuth.
	ctx, cancel := context.WithCancel(context.Background())
	attempt := &oauthAttempt{cancel: cancel, done: make(chan struct{})}
	c.mu.Lock()
	c.inflight[id] = attempt
	c.mu.Unlock()
	go func() {
		<-ctx.Done()
		c.clearInflight(id, attempt)
		close(attempt.done)
	}()

	returned := make(chan struct{})
	go func() { c.supersedeInflight(id); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("supersedeInflight did not return after cancelling the stale attempt")
	}

	c.mu.Lock()
	_, still := c.inflight[id]
	c.mu.Unlock()
	if still {
		t.Error("inflight entry still present after supersede")
	}

	// With nothing in flight, supersede is a no-op and must not block.
	done := make(chan struct{})
	go func() { c.supersedeInflight(id); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supersedeInflight blocked with no in-flight attempt")
	}
}

// clearInflight must only remove the attempt it owns: a finishing stale attempt
// must not evict the superseding one that already replaced it.
func TestOAuthCoordinatorClearInflightOnlyMatching(t *testing.T) {
	c := NewOAuthCoordinator(nil)
	const id = "srv"
	stale := &oauthAttempt{cancel: func() {}, done: make(chan struct{})}
	current := &oauthAttempt{cancel: func() {}, done: make(chan struct{})}

	c.mu.Lock()
	c.inflight[id] = current
	c.mu.Unlock()

	c.clearInflight(id, stale) // stale is not the current owner — must be ignored
	c.mu.Lock()
	got := c.inflight[id]
	c.mu.Unlock()
	if got != current {
		t.Fatal("clearInflight evicted a non-owning attempt")
	}

	c.clearInflight(id, current)
	c.mu.Lock()
	_, ok := c.inflight[id]
	c.mu.Unlock()
	if ok {
		t.Error("clearInflight did not remove the owning attempt")
	}
}
