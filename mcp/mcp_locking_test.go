package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCP_ToolListDoesNotHoldLockDuringFetch proves the blocking ListTools RPC
// runs outside s.mu: while a fetch is in flight, InvalidateToolsCache (which the
// list_changed handler calls, taking s.mu) must not block. With the lock held
// across the network call, the invalidation would deadlock behind the RPC.
func TestMCP_ToolListDoesNotHoldLockDuringFetch(t *testing.T) {
	ctx := context.Background()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo"}}}, nil, nil
		})

	var once sync.Once
	entered := make(chan struct{}) // closed once tools/list is in flight
	release := make(chan struct{}) // closed to let tools/list complete
	srv.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/list" {
				once.Do(func() { close(entered) })
				<-release
			}
			return next(ctx, method, req)
		}
	})

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	server, err := NewWithTransport(ctx, "test", clientT, Options{CacheToolsList: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc, ag := rcAg()
	listDone := make(chan error, 1)
	go func() {
		_, e := server.ListTools(ctx, rc, ag)
		listDone <- e
	}()

	// Wait until the ListTools RPC is blocked server-side.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tools/list never reached the server")
	}

	// The lock-sensitive operation must complete promptly while the fetch blocks.
	invDone := make(chan struct{})
	go func() {
		server.InvalidateToolsCache()
		close(invDone)
	}()
	select {
	case <-invDone:
		// Good: s.mu was free during the in-flight network call.
	case <-time.After(2 * time.Second):
		close(release) // unblock the RPC so goroutines can exit
		t.Fatal("InvalidateToolsCache blocked during an in-flight ListTools RPC; s.mu is held across the network call")
	}

	close(release)
	if err := <-listDone; err != nil {
		t.Fatalf("ListTools: %v", err)
	}
}
