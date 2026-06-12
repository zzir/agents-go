package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

type echoArgs struct {
	Text string `json:"text" jsonschema:"the text to echo"`
}

// startInProcessServer spins up an MCP server with one "echo" tool and returns a
// connected Server using an in-memory transport.
func startInProcessServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo the input"},
		func(ctx context.Context, req *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + args.Text}},
			}, nil, nil
		})

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	server, err := NewWithTransport(ctx, "test", clientT, Options{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

func TestMCP_ListAndCallTools(t *testing.T) {
	ctx := context.Background()
	server := startInProcessServer(t)

	tools, err := server.ListTools(ctx, agents.NewRunContext(nil), &agents.Agent{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	ft, ok := tools[0].(*agents.FunctionTool)
	if !ok || ft.Name != "echo" {
		t.Fatalf("unexpected tool: %+v", tools[0])
	}

	out, err := ft.OnInvoke(ctx, &agents.ToolContext{RunContext: agents.NewRunContext(nil)}, `{"text":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "echo: hi" {
		t.Errorf("tool output = %v, want 'echo: hi'", out)
	}
}

func TestMCP_ToolFiltering(t *testing.T) {
	ctx := context.Background()
	server := startInProcessServer(t)
	server.blocked["echo"] = true

	tools, err := server.ListTools(ctx, agents.NewRunContext(nil), &agents.Agent{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Errorf("blocked tool should be hidden, got %d tools", len(tools))
	}
}
