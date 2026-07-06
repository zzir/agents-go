package server

import (
	"context"
	"testing"
	"time"
)

// WriteAsync must never block the producer: once the bounded outbound queue is
// full (a stuck client whose writer goroutine can't drain), it returns false so
// the caller drops the connection instead of back-pressuring the hub/run
// goroutine that publishes events.
func TestWriteAsyncNeverBlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &WSConn{ctx: ctx, cancel: cancel, out: make(chan any, 2)}

	if !c.WriteAsync("a") || !c.WriteAsync("b") {
		t.Fatal("writes within capacity must succeed")
	}
	// Queue is full and nothing drains it: the next write must fail fast, not
	// block.
	done := make(chan bool, 1)
	go func() { done <- c.WriteAsync("c") }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("WriteAsync on a full queue must return false")
		}
	case <-time.After(time.Second):
		t.Fatal("WriteAsync blocked on a full queue — it must never block the producer")
	}

	// After the context is cancelled (connection closing), writes fail rather
	// than leak into a queue no one drains.
	cancel()
	if c.WriteAsync("d") {
		t.Fatal("WriteAsync after ctx cancel must return false")
	}
}

// A WSConn whose writer was never started drops async writes rather than
// panicking on a nil channel.
func TestWriteAsyncNilQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &WSConn{ctx: ctx, cancel: cancel}
	if c.WriteAsync("x") {
		t.Fatal("WriteAsync with no queue must return false")
	}
}
