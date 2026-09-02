package mcp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A request the server never answers is freed by the connection's request
// ceiling rather than pinned for the connection's lifetime: the request
// context ends, and a caller still waiting sees the deadline (decisions §5.20).
func TestCallSessionCeilingFreesAHungRequest(t *testing.T) {
	s := newServer("hung", Options{})
	t.Cleanup(s.rpcStop)
	s.requestCeiling = 50 * time.Millisecond

	var requestCtx context.Context
	_, err := callSession(context.Background(), s, func(rpc context.Context) (int, error) {
		requestCtx = rpc
		<-rpc.Done() // a server that never answers
		return 0, rpc.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the ceiling's DeadlineExceeded", err)
	}
	if s.rpcCtx.Err() != nil {
		t.Fatal("the ceiling ended the CONNECTION's context, not just the request's")
	}
	if requestCtx.Err() == nil {
		t.Fatal("the request context is still live after the ceiling fired")
	}
}

// A request that answers is unaffected by the ceiling, and the caller's own
// cancellation still returns at once with the request left to finish.
func TestCallSessionCeilingDoesNotTouchAnAnsweredRequest(t *testing.T) {
	s := newServer("quick", Options{})
	t.Cleanup(s.rpcStop)
	s.requestCeiling = time.Hour
	got, err := callSession(context.Background(), s, func(context.Context) (string, error) { return "pong", nil })
	if err != nil || got != "pong" {
		t.Fatalf("callSession = %q, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	_, err = callSession(ctx, s, func(context.Context) (string, error) { <-release; return "", nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller got %v, want context.Canceled", err)
	}
}
