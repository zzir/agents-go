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
	ft := tools[0]
	if ft.Name != "echo" {
		t.Fatalf("unexpected tool: %+v", tools[0])
	}

	out, err := ft.OnInvoke(ctx, &agents.ToolContext{RunContext: agents.NewRunContext(nil)}, `{"text":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.ModelOutput() != "echo: hi" {
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

// fixedResultTool registers a tool that ignores its arguments and answers with
// a fixed result, for exercising the adapted tool against exotic content blocks.
func fixedResultTool(srv *mcpsdk.Server, name string, res *mcpsdk.CallToolResult) {
	srv.AddTool(&mcpsdk.Tool{Name: name, Description: name, InputSchema: emptyObjectSchema()},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return res, nil
		})
}

// callExposedTool invokes an exposed tool by name through the adapted
// agents.Tool — the path a run takes, assembly layer included.
func callExposedTool(t *testing.T, server *Server, name, argsJSON string) agents.ToolResult {
	t.Helper()
	ctx := context.Background()
	rc, ag := rcAg()
	tools, err := server.ListTools(ctx, rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Name != name {
			continue
		}
		out, err := tl.OnInvoke(ctx, &agents.ToolContext{RunContext: rc}, argsJSON)
		if err != nil {
			t.Fatalf("call %q: %v", name, err)
		}
		return out
	}
	t.Fatalf("tool %q is not exposed", name)
	return agents.ToolResult{}
}

// A multi-block result reaches the model as native parts: text stays text and
// the image stays an image, rather than both being flattened into one
// JSON-encoded text part the model can only read as prose.
func TestMCP_MultiBlockResultIsNative(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47}
	server := startServer(t, Options{}, func(srv *mcpsdk.Server) {
		fixedResultTool(srv, "chart", &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "see chart"},
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: png},
		}})
	})

	out := callExposedTool(t, server, "chart", `{}`)
	if len(out.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2: %#v", len(out.Content), out.Content)
	}
	if tp, ok := out.Content[0].(agents.ToolOutputText); !ok || tp.Text != "see chart" {
		t.Errorf("Content[0] = %#v, want text %q", out.Content[0], "see chart")
	}
	img, ok := out.Content[1].(agents.ToolOutputImage)
	if !ok {
		t.Fatalf("Content[1] = %T, want ToolOutputImage", out.Content[1])
	}
	if want := agents.DataURL("image/png", png); img.ImageURL != want {
		t.Errorf("image URL = %q, want %q", img.ImageURL, want)
	}
	if out.IsError {
		t.Error("IsError = true, want false")
	}
}

// The single-text case stays a lone text part, which collapses back to a plain
// string for the model.
func TestMCP_SingleTextResultCollapses(t *testing.T) {
	server := startServer(t, Options{}, nil)

	out := callExposedTool(t, server, "echo", `{"text":"hi"}`)
	if got := singleText(t, out.Content); got != "echo: hi" {
		t.Errorf("text part = %q, want %q", got, "echo: hi")
	}
	if got := out.ModelOutput(); got != "echo: hi" {
		t.Errorf("ModelOutput() = %#v, want %q", got, "echo: hi")
	}
}

// An isError result carries its blocks natively too, and stays marked as an
// error so a UI can render it as one.
func TestMCP_ErrorResultKeepsNativeContent(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47}
	server := startServer(t, Options{}, func(srv *mcpsdk.Server) {
		fixedResultTool(srv, "boom", &mcpsdk.CallToolResult{
			IsError: true,
			Content: []mcpsdk.Content{
				&mcpsdk.TextContent{Text: "rendering failed, last frame:"},
				&mcpsdk.ImageContent{MIMEType: "image/png", Data: png},
			},
		})
	})

	out := callExposedTool(t, server, "boom", `{}`)
	if !out.IsError {
		t.Error("IsError = false, want true")
	}
	if len(out.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2: %#v", len(out.Content), out.Content)
	}
	if _, ok := out.Content[1].(agents.ToolOutputImage); !ok {
		t.Errorf("Content[1] = %T, want ToolOutputImage", out.Content[1])
	}
}

// singleText returns the text of a one-part text result, failing otherwise.
func singleText(t *testing.T, parts []agents.ToolOutputContent) string {
	t.Helper()
	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1: %#v", len(parts), parts)
	}
	tp, ok := parts[0].(agents.ToolOutputText)
	if !ok {
		t.Fatalf("parts[0] = %T, want ToolOutputText", parts[0])
	}
	return tp.Text
}

func TestResultOutput_TextOnly(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "hello"}}}
	if got := singleText(t, resultOutput(res, false)); got != "hello" {
		t.Fatalf("text-only result = %q, want %q", got, "hello")
	}
}

func TestResultOutput_Image(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "see chart"},
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{0x89, 0x50}},
	}}
	parts := resultOutput(res, false)
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
	parts := resultOutput(res, false)
	if len(parts) != 1 {
		t.Fatalf("resource image result = %#v", parts)
	}
	if _, ok := parts[0].(agents.ToolOutputImage); !ok {
		t.Errorf("parts[0] = %T, want ToolOutputImage", parts[0])
	}
}

// Multiple text blocks become a list of native text parts, not one
// JSON-encoded string.
func TestResultOutput_MultipleTextBlocks(t *testing.T) {
	res := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "first"},
		&mcpsdk.TextContent{Text: "second"},
	}}
	parts := resultOutput(res, false)
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

// A result with no content blocks still yields one (empty) text part, so a tool
// whose effect is the point sends an empty string to the model.
func TestResultOutput_NoBlocks(t *testing.T) {
	if got := singleText(t, resultOutput(&mcpsdk.CallToolResult{}, false)); got != "" {
		t.Errorf("empty result = %q, want the empty string", got)
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
	if got := singleText(t, resultOutput(res, false)); got != "block text" {
		t.Errorf("default output = %q, want the content block text", got)
	}
	// Opted in: use structuredContent exclusively.
	if got := singleText(t, resultOutput(res, true)); !strings.Contains(got, "42") {
		t.Errorf("useStructured output = %q, want the structured JSON", got)
	}
}

// When UseStructuredContent is opted in but the result carries no (or empty)
// structuredContent, the output must fall back to the content blocks rather
// than blanking out.
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
			if got := singleText(t, resultOutput(res, true)); got != "fallback text" {
				t.Errorf("output = %q, want the content block text (structured was empty)", got)
			}
		})
	}
}

// With opt-in and empty structuredContent AND no content blocks, the result is
// an empty string (nothing to fall back to) — never a panic.
func TestResultOutput_StructuredContentEmptyNoBlocks(t *testing.T) {
	res := &mcpsdk.CallToolResult{StructuredContent: map[string]any{}}
	if got := singleText(t, resultOutput(res, true)); got != "" {
		t.Errorf("output = %q, want empty string", got)
	}
}

// A scalar structured value (e.g. a JSON number) counts as present and is used
// exclusively, not discarded as "empty".
func TestResultOutput_StructuredContentScalarIsUsed(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "block"}},
		StructuredContent: 42.0,
	}
	if got := singleText(t, resultOutput(res, true)); got != "42" {
		t.Errorf("output = %q, want the structured scalar \"42\"", got)
	}
}
