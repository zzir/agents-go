package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

// --- retry -----------------------------------------------------------

func TestMCP_RunWithRetries_SucceedsAfterFailures(t *testing.T) {
	s := newServer("t", Options{MaxRetryAttempts: 3, RetryBackoffBase: time.Millisecond})
	calls := 0
	err := s.runWithRetries(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestMCP_RunWithRetries_NoRetriesByDefault(t *testing.T) {
	s := newServer("t", Options{})
	calls := 0
	err := s.runWithRetries(context.Background(), func() error {
		calls++
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("expected 1 attempt with no retries, got %d", calls)
	}
}

func TestMCP_RunWithRetries_Infinite(t *testing.T) {
	s := newServer("t", Options{MaxRetryAttempts: -1, RetryBackoffBase: time.Microsecond})
	calls := 0
	err := s.runWithRetries(context.Background(), func() error {
		calls++
		if calls < 6 {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil || calls != 6 {
		t.Fatalf("infinite retry: err=%v calls=%d", err, calls)
	}
}

func TestMCP_RunWithRetries_ContextCancel(t *testing.T) {
	s := newServer("t", Options{MaxRetryAttempts: -1, RetryBackoffBase: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.runWithRetries(ctx, func() error { return errors.New("boom") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// --- name prefixing / truncation / dedup -----------------------------

func TestMCP_ExposedNames_PlainPrefix(t *testing.T) {
	s := newServer("srv", Options{ToolNamePrefix: "gh_"})
	names := s.exposedNames([]*mcpsdk.Tool{{Name: "a"}, {Name: "b"}})
	if names[0] != "gh_a" || names[1] != "gh_b" {
		t.Fatalf("plain prefix wrong: %v", names)
	}
}

func TestMCP_ExposedNames_ServerPrefix(t *testing.T) {
	s := newServer("srv", Options{IncludeServerInToolNames: true})
	names := s.exposedNames([]*mcpsdk.Tool{{Name: "echo"}})
	if names[0] != "mcp_srv__echo" {
		t.Fatalf("server prefix wrong: %v", names)
	}
}

func TestMCP_ExposedNames_Truncation(t *testing.T) {
	s := newServer("srv", Options{IncludeServerInToolNames: true})
	long := strings.Repeat("x", 80)
	names := s.exposedNames([]*mcpsdk.Tool{{Name: long}})
	if len(names[0]) > mcpToolNameMaxLength {
		t.Fatalf("name not truncated: len=%d", len(names[0]))
	}
	// Truncated names carry an "_<8 hex>" sha1 suffix.
	if idx := strings.LastIndexByte(names[0], '_'); idx < 0 || len(names[0])-idx-1 != mcpToolHashLength {
		t.Fatalf("expected sha1 suffix, got %q", names[0])
	}
}

func TestMCP_ExposedNames_CollisionDedup(t *testing.T) {
	s := newServer("srv", Options{IncludeServerInToolNames: true})
	// Both sanitize to the same base name "mcp_srv__foo_bar" and must be
	// disambiguated with distinct hash suffixes.
	names := s.exposedNames([]*mcpsdk.Tool{{Name: "foo.bar"}, {Name: "foo_bar"}})
	if names[0] == names[1] {
		t.Fatalf("collision not disambiguated: %v", names)
	}
	for _, n := range names {
		if len(n) > mcpToolNameMaxLength {
			t.Fatalf("deduped name too long: %q", n)
		}
	}
}

// --- required-arg pre-validation -------------------------------------

func TestMCP_RequiredArgValidation(t *testing.T) {
	server := startInProcessServer(t)
	rc, ag := rcAg()
	tools, err := server.ListTools(context.Background(), rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	ft := tools[0].(*agents.FunctionTool)
	tc := &agents.ToolContext{RunContext: rc}
	// "text" is required; an empty object must fail with a *UserError before the
	// server is ever contacted.
	_, err = ft.OnInvoke(context.Background(), tc, `{}`)
	var ue *agents.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *agents.UserError, got %v", err)
	}
	if !strings.Contains(ue.Error(), "missing required parameters: text") {
		t.Fatalf("unexpected message: %v", ue)
	}
	// A valid call still works.
	out, err := ft.OnInvoke(context.Background(), tc, `{"text":"hi"}`)
	if err != nil || out.ModelOutput() != "echo: hi" {
		t.Fatalf("valid call failed: out=%v err=%v", out, err)
	}
}

func TestValidateRequiredArgs(t *testing.T) {
	if err := validateRequiredArgs("s", "t", nil, map[string]any{}); err != nil {
		t.Fatalf("no required keys should pass: %v", err)
	}
	err := validateRequiredArgs("s", "t", []string{"b", "a"}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "missing required parameters: a, b") {
		t.Fatalf("expected sorted missing keys, got %v", err)
	}
	if err := validateRequiredArgs("s", "t", []string{"a"}, map[string]any{"a": 1}); err != nil {
		t.Fatalf("present key should pass: %v", err)
	}
}

// --- tool _meta ------------------------------------------------------

func TestMCP_ToolMeta(t *testing.T) {
	// The metaEcho tool returns whatever _meta it received, as JSON text.
	server := startServer(t, Options{
		ToolMetaResolver: func(_ context.Context, _ *agents.RunContext, tool string, _ map[string]any) (map[string]any, error) {
			return map[string]any{"from": "resolver", "tool": tool, "shared": "resolver"}, nil
		},
	}, func(srv *mcpsdk.Server) {
		mcpsdk.AddTool(srv, &mcpsdk.Tool{
			Name: "meta_echo",
			// A static _meta on the tool overrides the resolver on key collisions.
			Meta: mcpsdk.Meta{"static": true, "shared": "static"},
		}, func(_ context.Context, req *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
			b, _ := json.Marshal(map[string]any(req.Params.Meta))
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(b)}}}, nil, nil
		})
	})

	rc, ag := rcAg()
	tools, err := server.ListTools(context.Background(), rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	var ft *agents.FunctionTool
	for _, tl := range tools {
		if f := tl.(*agents.FunctionTool); f.Name == "meta_echo" {
			ft = f
		}
	}
	if ft == nil {
		t.Fatal("meta_echo not listed")
	}
	out, err := ft.OnInvoke(context.Background(), &agents.ToolContext{RunContext: rc}, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out.ModelOutput().(string)), &got); err != nil {
		t.Fatalf("meta not JSON: %v (%v)", err, out)
	}
	if got["from"] != "resolver" || got["static"] != true {
		t.Fatalf("merged meta missing entries: %v", got)
	}
	if got["shared"] != "static" {
		t.Fatalf("static _meta should override resolver on collision, got %v", got["shared"])
	}
}

// --- dynamic require_approval ----------------------------------------

func TestMCP_DynamicRequireApproval(t *testing.T) {
	server := startServer(t, Options{
		RequireApprovalFunc: func(_ context.Context, _ *agents.RunContext, agent *agents.Agent, tool string) bool {
			return agent.Name == "needs" && tool == "echo"
		},
	}, nil)

	rc := agents.NewRunContext(nil)
	// Agent "needs" → approval required.
	tools, err := server.ListTools(context.Background(), rc, &agents.Agent{Name: "needs"})
	if err != nil {
		t.Fatal(err)
	}
	ft := tools[0].(*agents.FunctionTool)
	if ft.NeedsApprovalFunc == nil {
		t.Fatal("expected NeedsApprovalFunc wired")
	}
	if need, _ := ft.NeedsApprovalFunc(context.Background(), rc, "", ""); !need {
		t.Fatal("agent 'needs' should require approval")
	}
	// A different agent captured per ListTools call → no approval.
	tools2, _ := server.ListTools(context.Background(), rc, &agents.Agent{Name: "other"})
	ft2 := tools2[0].(*agents.FunctionTool)
	if need, _ := ft2.NeedsApprovalFunc(context.Background(), rc, "", ""); need {
		t.Fatal("agent 'other' should not require approval")
	}
}

// --- description fallback --------------------------------------------

func TestMCP_DescriptionFallback(t *testing.T) {
	server := startServer(t, Options{}, func(srv *mcpsdk.Server) {
		mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "titled", Title: "My Title"},
			func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
				return &mcpsdk.CallToolResult{}, nil, nil
			})
	})
	rc, ag := rcAg()
	tools, err := server.ListTools(context.Background(), rc, ag)
	if err != nil {
		t.Fatal(err)
	}
	var ft *agents.FunctionTool
	for _, tl := range tools {
		if f := tl.(*agents.FunctionTool); f.Name == "titled" {
			ft = f
		}
	}
	if ft == nil {
		t.Fatal("titled tool not listed")
	}
	if ft.Description != "My Title" {
		t.Fatalf("description should fall back to title, got %q", ft.Description)
	}
}

func TestResolveToolDescription(t *testing.T) {
	if got := resolveToolDescription(&mcpsdk.Tool{Description: "d", Title: "t"}); got != "d" {
		t.Fatalf("description wins: %q", got)
	}
	if got := resolveToolDescription(&mcpsdk.Tool{Title: "t"}); got != "t" {
		t.Fatalf("title fallback: %q", got)
	}
	if got := resolveToolDescription(&mcpsdk.Tool{Annotations: &mcpsdk.ToolAnnotations{Title: "at"}}); got != "at" {
		t.Fatalf("annotations title fallback: %q", got)
	}
	if got := resolveToolDescription(&mcpsdk.Tool{}); got != "" {
		t.Fatalf("empty: %q", got)
	}
}
