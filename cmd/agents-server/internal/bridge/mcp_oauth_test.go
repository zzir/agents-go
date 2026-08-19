package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// TestHandleCallbackIdempotentAndNonBlocking locks: a duplicate OAuth
// callback must neither double-deliver nor block forever on a full, unread
// channel. The pending entry is consumed under the lock on the first callback,
// and the delivery send is non-blocking.
func TestHandleCallbackIdempotentAndNonBlocking(t *testing.T) {
	c := NewOAuthCoordinator(nil)

	// (1) Idempotency: the first callback delivers and consumes the entry; a
	// second for the same state finds nothing instead of racing or re-delivering.
	const state = "st-1"
	codeCh := make(chan *auth.AuthorizationResult, 1)
	c.mu.Lock()
	c.pending[state] = codeCh
	c.mu.Unlock()

	if err := c.HandleCallback(state, "code-1", ""); err != nil {
		t.Fatalf("first callback: %v", err)
	}
	if got := <-codeCh; got.Code != "code-1" {
		t.Fatalf("delivered code = %q, want code-1", got.Code)
	}
	if err := c.HandleCallback(state, "code-2", ""); err == nil {
		t.Fatal("duplicate callback should report unknown/expired state (consumed once)")
	}

	// (2) Non-blocking: even with a full, unread channel (the fetcher already
	// gave up), delivery must not park the goroutine forever.
	const state2 = "st-2"
	full := make(chan *auth.AuthorizationResult, 1)
	full <- &auth.AuthorizationResult{} // pre-fill: no capacity, no receiver
	c.mu.Lock()
	c.pending[state2] = full
	c.mu.Unlock()

	done := make(chan struct{})
	go func() { _ = c.HandleCallback(state2, "code-3", ""); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleCallback blocked on a full channel ")
	}
}

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
