package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
)

// ServeOptions configures an MCP server.
type ServeOptions struct {
	// Name and Version identify the server to clients.
	Name, Version string
	// Instructions tell a client what this server is for; MCP surfaces them
	// alongside the tool list.
	Instructions string
}

func (o ServeOptions) withDefaults() ServeOptions {
	o.Name = cmp.Or(o.Name, "agents-go")
	o.Version = cmp.Or(o.Version, "0.1.0")
	return o
}

// NewToolServer exposes SDK tools over MCP.
//
// It is the direction this package did not go before: `mcp.Server` connects to
// somebody else's tools, and this hands ours to somebody else — an editor, a
// desktop client, another agent. The tools are the same values an Agent runs,
// so a capability written once is available in both places instead of being
// reimplemented for each.
//
// The caller owns the transport; see ServeStdio for the common case.
func NewToolServer(tools []*agents.Tool, opts ServeOptions) (*mcpsdk.Server, error) {
	opts = opts.withDefaults()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    opts.Name,
		Version: opts.Version,
	}, &mcpsdk.ServerOptions{Instructions: opts.Instructions})

	for _, t := range tools {
		tool, handler, err := exportTool(t)
		if err != nil {
			return nil, err
		}
		srv.AddTool(tool, handler)
	}
	return srv, nil
}

// exportTool converts one SDK tool into its MCP declaration and handler.
func exportTool(t *agents.Tool) (*mcpsdk.Tool, mcpsdk.ToolHandler, error) {
	name := t.Name
	if t.OnInvoke == nil {
		return nil, nil, fmt.Errorf("mcp: tool %q has no OnInvoke and cannot be served", name)
	}

	schema, err := json.Marshal(t.ParamsJSONSchema)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: encoding schema for tool %q: %w", name, err)
	}
	decl := &mcpsdk.Tool{
		Name:        name,
		Description: t.Description,
		InputSchema: json.RawMessage(schema),
	}

	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		args := "{}"
		if len(req.Params.Arguments) > 0 {
			args = string(req.Params.Arguments)
		}
		// A tool invoked over MCP has no run behind it: no session, no usage
		// accounting, no approvals. The ToolContext carries what exists — the
		// call's identity — and nothing it would have to invent.
		tc := &agents.ToolContext{
			RunContext:    agents.NewRunContext(nil),
			ToolName:      name,
			ToolArguments: args,
		}
		res, err := t.OnInvoke(ctx, tc, args)
		if err != nil {
			// A tool failure is a RESULT, not a protocol error: the caller is a
			// model, and it can act on "that path does not exist" while a
			// transport error only tells it the connection is fine.
			//nolint:nilerr // deliberate: the failure travels in the result
			return &mcpsdk.CallToolResult{
				IsError: true,
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
			}, nil
		}
		return &mcpsdk.CallToolResult{
			IsError: res.IsError,
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: stringifyOutput(res.ModelOutput())}},
		}, nil
	}
	return decl, handler, nil
}

// NewAgentServer exposes a whole agent as a single MCP tool.
//
// The tool takes one string and returns the agent's final output, which is what
// an editor or another agent actually wants: it is asking a question, not
// driving a turn loop. The agent's own tools stay inside — they are how it
// answers, not what it offers.
func NewAgentServer(a *agents.Agent, runOpts agents.RunOptions, opts ServeOptions) (*mcpsdk.Server, error) {
	if a == nil {
		return nil, fmt.Errorf("mcp: NewAgentServer requires an agent")
	}
	// The agent names the server when the caller did not — before withDefaults,
	// so a caller who explicitly asks for the default name keeps it.
	if opts.Name == "" {
		opts.Name = a.Name
	}
	opts = opts.withDefaults()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    opts.Name,
		Version: opts.Version,
	}, &mcpsdk.ServerOptions{Instructions: opts.Instructions})

	description := a.HandoffDescription
	if description == "" {
		description = "Ask " + a.Name + "."
	}
	srv.AddTool(&mcpsdk.Tool{
		Name:        toolNameFor(a.Name),
		Description: description,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"What to ask the agent."}},"required":["input"],"additionalProperties":false}`),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			Input string `json:"input"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				//nolint:nilerr // the caller is a model; bad arguments are its to fix
				return &mcpsdk.CallToolResult{
					IsError: true,
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "invalid arguments: " + err.Error()}},
				}, nil
			}
		}
		res, err := agents.RunSync(ctx, a, args.Input, runOpts)
		if err != nil {
			//nolint:nilerr // a failed run is an answer the caller can act on
			return &mcpsdk.CallToolResult{
				IsError: true,
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
			}, nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: res.FinalOutputString()}},
		}, nil
	})
	return srv, nil
}

// ServeStdio runs a server over stdin/stdout, which is how editors launch one.
func ServeStdio(ctx context.Context, srv *mcpsdk.Server) error {
	return srv.Run(ctx, &mcpsdk.StdioTransport{})
}

// toolNameFor sanitizes an agent name into a tool name: MCP clients key on it,
// and a name with spaces is one some of them will not call.
func toolNameFor(name string) string {
	if name == "" {
		return "ask_agent"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return "ask_" + b.String()
}

// stringifyOutput renders a tool result for a text content block.
func stringifyOutput(v any) string {
	switch out := v.(type) {
	case nil:
		return ""
	case string:
		return out
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprint(v)
}
