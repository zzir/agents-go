package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/mcp"
)

// connect wires a server to an in-process client, which is what a real client
// does over stdio minus the pipes.
func connect(t *testing.T, srv *mcpsdk.Server) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() {
		if err := srv.Run(ctx, st); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

type greetArgs struct {
	Name string `json:"name" jsonschema:"who to greet"`
}

// A capability written once should be available to an agent AND to an editor,
// rather than reimplemented for each.
func TestServeTools_ExposesAnSDKTool(t *testing.T) {
	ctx := context.Background()
	greet := agents.NewTool("greet", "Greet somebody.",
		func(_ context.Context, _ *agents.ToolContext, a greetArgs) (string, error) {
			return "hello " + a.Name, nil
		})

	srv, err := mcp.NewToolServer([]*agents.Tool{greet}, mcp.ServeOptions{Name: "test-server"})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	list, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "greet" {
		t.Fatalf("tools = %+v, want greet", list.Tools)
	}
	if list.Tools[0].Description != "Greet somebody." {
		t.Errorf("description = %q", list.Tools[0].Description)
	}
	// The schema travels, so a client can validate before calling.
	if list.Tools[0].InputSchema == nil {
		t.Error("the tool was served without its schema")
	}

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "greet",
		Arguments: map[string]any{"name": "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(res); got != "hello world" {
		t.Errorf("result = %q, want the tool's output", got)
	}
}

// A tool failure is a RESULT, not a protocol error: the caller is a model, and
// it can act on "that path does not exist" while a transport error only tells
// it the connection is fine.
func TestServeTools_ToolFailureIsAResult(t *testing.T) {
	ctx := context.Background()
	boom := agents.NewTool("boom", "Always fails.",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			return "", errors.New("no such path")
		})
	boom.FailureErrorFunction = nil // make it a hard failure

	srv, err := mcp.NewToolServer([]*agents.Tool{boom}, mcp.ServeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "boom", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("a tool failure surfaced as a protocol error: %v", err)
	}
	if !res.IsError {
		t.Error("the failure was not marked as an error")
	}
	if !strings.Contains(textOf(res), "no such path") {
		t.Errorf("result = %q, want the reason", textOf(res))
	}
}

// An editor asking a question wants the answer, not a turn loop: the agent's
// own tools stay inside.
func TestServeAgent_ExposesOneAskTool(t *testing.T) {
	ctx := context.Background()
	inner := agents.NewTool("secret", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "", nil
	})
	agent := &agents.Agent{
		Name:               "Research Bot",
		HandoffDescription: "Answers research questions.",
		Tools:              []*agents.Tool{inner},
		ModelImpl:          &scriptedModel{text: "the answer is 42"},
	}

	srv, err := mcp.NewAgentServer(agent, agents.RunOptions{}, mcp.ServeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	list, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 {
		t.Fatalf("tools = %+v, want exactly one ask tool", list.Tools)
	}
	// Sanitized: a name with spaces is one some clients will not call.
	if list.Tools[0].Name != "ask_research_bot" {
		t.Errorf("tool name = %q", list.Tools[0].Name)
	}
	if list.Tools[0].Description != "Answers research questions." {
		t.Errorf("description = %q", list.Tools[0].Description)
	}

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "ask_research_bot",
		Arguments: map[string]any{"input": "what is it?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(res); got != "the answer is 42" {
		t.Errorf("result = %q, want the agent's final output", got)
	}
}

func TestServeAgent_RunFailureIsAResult(t *testing.T) {
	ctx := context.Background()
	agent := &agents.Agent{Name: "a", ModelImpl: &scriptedModel{err: errors.New("provider down")}}

	srv, err := mcp.NewAgentServer(agent, agents.RunOptions{}, mcp.ServeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, srv)

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "ask_a", Arguments: map[string]any{"input": "hi"},
	})
	if err != nil {
		t.Fatalf("a run failure surfaced as a protocol error: %v", err)
	}
	if !res.IsError || !strings.Contains(textOf(res), "provider down") {
		t.Errorf("result = %+v, want the failure reported", res)
	}
}

func textOf(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// scriptedModel answers with one fixed message, or fails.
type scriptedModel struct {
	text string
	err  error
}

func (m *scriptedModel) Respond(context.Context, agents.ModelRequest) (*agents.ModelResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":` +
		quote(m.text) + `,"annotations":[]}]}`
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return nil, err
	}
	return &agents.ModelResponse{Output: []agents.OutputItem{item}, Usage: agents.NewUsage()}, nil
}

func (m *scriptedModel) StreamResponse(context.Context, agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		yield(nil, errors.New("streaming not used in these tests"))
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
