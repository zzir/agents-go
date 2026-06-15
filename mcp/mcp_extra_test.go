package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

// startServer spins up an MCP server with one "echo" tool and the given client
// Options, after letting configure register extra tools/prompts/resources.
func startServer(t *testing.T, opts Options, configure func(*mcpsdk.Server)) *Server {
	t.Helper()
	ctx := context.Background()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo the input"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + args.Text}}}, nil, nil
		})
	if configure != nil {
		configure(srv)
	}
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	server, err := NewWithTransport(ctx, "test", clientT, opts)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func rcAg() (*agents.RunContext, *agents.Agent) {
	return agents.NewRunContext(nil), &agents.Agent{Name: "a"}
}

func TestMCP_DynamicToolFilter(t *testing.T) {
	ctx := context.Background()
	server := startServer(t, Options{
		ToolFilter: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, name string) bool {
			return name != "echo"
		},
	}, nil)
	rc, ag := rcAg()
	tools, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Errorf("dynamic filter should hide echo, got %d tools", len(tools))
	}
}

func TestMCP_ToolNamePrefix(t *testing.T) {
	ctx := context.Background()
	server := startServer(t, Options{ToolNamePrefix: "test_"}, nil)
	rc, ag := rcAg()
	tools, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	ft := tools[0].(*agents.FunctionTool)
	if ft.Name != "test_echo" {
		t.Errorf("exposed name = %q, want test_echo", ft.Name)
	}
	// The server is still invoked with the original (unprefixed) name.
	out, err := ft.OnInvoke(ctx, &agents.ToolContext{RunContext: rc}, `{"text":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "echo: hi" {
		t.Errorf("call output = %v, want 'echo: hi'", out)
	}
}

func TestMCP_RequireApproval(t *testing.T) {
	ctx := context.Background()
	server := startServer(t, Options{
		RequireApproval: func(name string) bool { return name == "echo" },
	}, nil)
	rc, ag := rcAg()
	tools, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	if ft := tools[0].(*agents.FunctionTool); !ft.NeedsApproval {
		t.Error("echo should require approval")
	}
}

func TestMCP_CacheToolsList(t *testing.T) {
	ctx := context.Background()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo"}}}, nil, nil
		})
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	server, err := NewWithTransport(ctx, "test", clientT, Options{CacheToolsList: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	rc, ag := rcAg()
	first, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("got %d tools, want 1", len(first))
	}

	// Add a tool server-side; the cache should hide it until invalidated.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo2", Description: "echo2"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, args echoArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{}, nil, nil
		})

	cached, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 {
		t.Errorf("cached ListTools = %d tools, want 1 (cache hit)", len(cached))
	}

	server.InvalidateToolsCache()
	fresh, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 2 {
		t.Errorf("after invalidate = %d tools, want 2", len(fresh))
	}
}

func TestMCP_PromptsAndResources(t *testing.T) {
	ctx := context.Background()
	server := startServer(t, Options{}, func(srv *mcpsdk.Server) {
		srv.AddPrompt(&mcpsdk.Prompt{Name: "greet", Description: "a greeting"},
			func(_ context.Context, _ *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				return &mcpsdk.GetPromptResult{
					Messages: []*mcpsdk.PromptMessage{{Role: "user", Content: &mcpsdk.TextContent{Text: "Hello!"}}},
				}, nil
			})
		srv.AddResource(&mcpsdk.Resource{URI: "file:///doc.txt", Name: "doc", MIMEType: "text/plain"},
			func(_ context.Context, _ *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				return &mcpsdk.ReadResourceResult{
					Contents: []*mcpsdk.ResourceContents{{URI: "file:///doc.txt", MIMEType: "text/plain", Text: "content"}},
				}, nil
			})
	})

	prompts, err := server.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "greet" {
		t.Errorf("prompts = %+v", prompts.Prompts)
	}
	gp, err := server.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: "greet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(gp.Messages) != 1 {
		t.Errorf("prompt messages = %d, want 1", len(gp.Messages))
	}

	resources, err := server.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 1 {
		t.Errorf("resources = %d, want 1", len(resources.Resources))
	}
	rr, err := server.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "file:///doc.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.Contents) != 1 || rr.Contents[0].Text != "content" {
		t.Errorf("resource contents = %+v", rr.Contents)
	}
}
