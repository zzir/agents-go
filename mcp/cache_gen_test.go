package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCP_InvalidateDuringFetchNotCached locks the generation guard: an
// InvalidateToolsCache that runs WHILE a ListTools fetch is in flight must stop
// that fetch's result from being cached. Otherwise a tools/list_changed firing
// mid-fetch is lost — the stale pre-change list gets written back over the
// just-cleared cache and the new tools are never seen. We block the first
// tools/list, invalidate during it, release it, then assert a SECOND ListTools
// still issues a fresh RPC (the stale result was not cached).
func TestMCP_InvalidateDuringFetchNotCached(t *testing.T) {
	ctx := context.Background()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo"}}}, nil, nil
		})

	var mu sync.Mutex
	listCalls := 0
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/list" {
				mu.Lock()
				listCalls++
				first := listCalls == 1
				mu.Unlock()
				if first {
					once.Do(func() { close(entered) })
					<-release
				}
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
	fetch1 := make(chan error, 1)
	go func() {
		_, e := server.ListTools(ctx, rc, ag)
		fetch1 <- e
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first tools/list never reached the server")
	}
	// Invalidate WHILE the first fetch is blocked in flight, then let it finish.
	server.InvalidateToolsCache()
	close(release)
	if e := <-fetch1; e != nil {
		t.Fatalf("first ListTools: %v", e)
	}

	// The mid-fetch invalidation must have prevented caching, so a second call
	// issues a fresh RPC rather than serving the stale pre-invalidation list.
	if _, e := server.ListTools(ctx, rc, ag); e != nil {
		t.Fatalf("second ListTools: %v", e)
	}
	mu.Lock()
	got := listCalls
	mu.Unlock()
	if got < 2 {
		t.Fatalf("second ListTools served a stale cache (tools/list RPC count = %d, want >= 2)", got)
	}
}
