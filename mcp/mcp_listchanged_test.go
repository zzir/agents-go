package mcp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

// TestMCP_CacheHitDoesNotRefetch pins the cache-hit half of CacheToolsList
// deterministically: with no server-side changes (hence no list_changed
// notification), repeated ListTools calls must hit the wire exactly once.
func TestMCP_CacheHitDoesNotRefetch(t *testing.T) {
	ctx := context.Background()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo"}}}, nil, nil
		})
	var listCalls atomic.Int64
	srv.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/list" {
				listCalls.Add(1)
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
	for range 3 {
		tools, err := server.ListTools(ctx, rc, ag)
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) != 1 {
			t.Fatalf("got %d tools, want 1", len(tools))
		}
	}
	if n := listCalls.Load(); n != 1 {
		t.Errorf("tools/list hit the wire %d times, want 1 (cache hit)", n)
	}
}

// TestMCP_EmptyArgumentsSentAsEmptyObject pins the wire format of zero-argument
// tool calls: "arguments" must be an explicit {} — matching the Python SDK,
// which sends an empty dict — not an omitted field, because some servers
// reject calls without an arguments key.
func TestMCP_EmptyArgumentsSentAsEmptyObject(t *testing.T) {
	ctx := context.Background()
	var gotArgs atomic.Value // raw JSON of params.arguments as the server received it
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	srv.AddTool(&mcpsdk.Tool{Name: "noargs", Description: "takes no arguments", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			// Arguments is the raw wire JSON; nil/"" here means the field was omitted.
			gotArgs.Store(string(req.Params.Arguments))
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		})

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	server, err := NewWithTransport(ctx, "test", clientT, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc, ag := rcAg()
	tools, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	ft := tools[0]

	for _, argsJSON := range []string{"", "   ", "null", "{}"} {
		gotArgs.Store("(unset)")
		if _, err := ft.OnInvoke(ctx, &agents.ToolContext{RunContext: rc}, argsJSON); err != nil {
			t.Fatalf("argsJSON %q: %v", argsJSON, err)
		}
		if got := gotArgs.Load().(string); got != "{}" {
			t.Errorf("argsJSON %q: wire arguments = %q, want %q", argsJSON, got, "{}")
		}
	}
}
