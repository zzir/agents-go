package mcp

import (
	"context"
	"strings"
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
	t.Cleanup(func() { _ = server.Close() })
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

func TestResultOutput_TextOnly(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "hello"}}}
	out := resultOutput(res, false)
	if s, ok := out.(string); !ok || s != "hello" {
		t.Fatalf("text-only result = %#v, want string \"hello\"", out)
	}
}

func TestResultOutput_Image(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "see chart"},
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{0x89, 0x50}},
	}}
	out := resultOutput(res, false)
	parts, ok := out.([]agents.ToolOutputContent)
	if !ok {
		t.Fatalf("image result is not structured content: %T", out)
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d", len(parts))
	}
	if _, ok := parts[0].(agents.ToolOutputText); !ok {
		t.Errorf("parts[0] = %T, want ToolOutputText", parts[0])
	}
	img, ok := parts[1].(agents.ToolOutputImage)
	if !ok {
		t.Fatalf("parts[1] = %T, want ToolOutputImage", parts[1])
	}
	if img.ImageURL == "" || img.ImageURL[:5] != "data:" {
		t.Errorf("image URL = %q", img.ImageURL)
	}
}

func TestResultOutput_ImageResource(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{
			URI: "file:///c.png", MIMEType: "image/png", Blob: []byte{1, 2, 3},
		}},
	}}
	out := resultOutput(res, false)
	parts, ok := out.([]agents.ToolOutputContent)
	if !ok || len(parts) != 1 {
		t.Fatalf("resource image result = %#v", out)
	}
	if _, ok := parts[0].(agents.ToolOutputImage); !ok {
		t.Errorf("parts[0] = %T, want ToolOutputImage", parts[0])
	}
}

// Multiple text blocks become a list of native text parts, not one
// JSON-encoded string (Python parity).
func TestResultOutput_MultipleTextBlocks(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "first"},
		&mcpsdk.TextContent{Text: "second"},
	}}
	out := resultOutput(res, false)
	parts, ok := out.([]agents.ToolOutputContent)
	if !ok {
		t.Fatalf("multi-block result = %T, want []ToolOutputContent parts", out)
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}
	for i, want := range []string{"first", "second"} {
		tp, ok := parts[i].(agents.ToolOutputText)
		if !ok || tp.Text != want {
			t.Errorf("parts[%d] = %#v, want text %q", i, parts[i], want)
		}
	}
}

// StructuredContent is ignored by default (the content blocks win) and
// used exclusively only when opted in.
func TestResultOutput_StructuredContentGating(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "block text"}},
		StructuredContent: map[string]any{"value": 42},
	}
	// Default: ignore structuredContent, use the content block.
	if out := resultOutput(res, false); out != "block text" {
		t.Errorf("default output = %#v, want the content block text", out)
	}
	// Opted in: use structuredContent exclusively.
	out := resultOutput(res, true)
	s, ok := out.(string)
	if !ok || !strings.Contains(s, "42") {
		t.Errorf("useStructured output = %#v, want the structured JSON", out)
	}
}

// When UseStructuredContent is opted in but the result carries no (or empty)
// structuredContent, the output must fall back to the content blocks rather
// than blanking out — mirroring the Python SDK's
// `if use_structured_content and result.structuredContent:` truthiness.
func TestResultOutput_StructuredContentEmptyFallsBackToBlocks(t *testing.T) {
	cases := []struct {
		name       string
		structured any
	}{
		{"nil", nil},
		{"empty map", map[string]any{}},
		{"empty slice", []any{}},
		{"empty string", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &mcpsdk.CallToolResult{
				Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "fallback text"}},
				StructuredContent: tc.structured,
			}
			if out := resultOutput(res, true); out != "fallback text" {
				t.Errorf("output = %#v, want the content block text (structured was empty)", out)
			}
		})
	}
}

// With opt-in and empty structuredContent AND no content blocks, the result is
// an empty string (nothing to fall back to) — never a panic.
func TestResultOutput_StructuredContentEmptyNoBlocks(t *testing.T) {
	res := &mcpsdk.CallToolResult{StructuredContent: map[string]any{}}
	if out := resultOutput(res, true); out != "" {
		t.Errorf("output = %#v, want empty string", out)
	}
}

// A scalar structured value (e.g. a JSON number) counts as present and is used
// exclusively, not discarded as "empty".
func TestResultOutput_StructuredContentScalarIsUsed(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "block"}},
		StructuredContent: 42.0,
	}
	out := resultOutput(res, true)
	if s, ok := out.(string); !ok || s != "42" {
		t.Errorf("output = %#v, want the structured scalar \"42\"", out)
	}
}
