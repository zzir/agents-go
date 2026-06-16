# Model context protocol (MCP)

[MCP](https://modelcontextprotocol.io/) is an open protocol for exposing tools (and other capabilities) to LLM applications. The `mcp` package connects an agent to MCP servers over the official [Go SDK](https://github.com/modelcontextprotocol/go-sdk): each server tool becomes a function tool the model can call.

## Connecting a server

```go
import (
	"os/exec"

	"github.com/zzir/agents-go/mcp"
)

// stdio: launch the server as a subprocess
fsServer, err := mcp.NewStdioServer(ctx, "filesystem",
	exec.Command("npx", "-y", "@modelcontextprotocol/server-filesystem", dir),
	mcp.Options{})
if err != nil { … }
defer fsServer.Close()

agent := &agents.Agent{
	Name:       "file assistant",
	MCPServers: []agents.MCPServer{fsServer},
}
```

Transports:

| Constructor | Transport |
|---|---|
| `mcp.NewStdioServer(ctx, name, cmd, opts)` | Subprocess over stdio |
| `mcp.NewStreamableHTTPServer(ctx, name, endpoint, opts)` | Streamable HTTP |
| `mcp.NewSSEServer(ctx, name, endpoint, opts)` | Server-sent events |
| `mcp.NewWithTransport(ctx, name, transport, opts)` | Anything implementing the go-sdk `Transport` (e.g. in-memory for tests) |

The agent lists each server's tools at the start of every turn, so servers may add or remove tools between turns.

## Options

```go
mcp.Options{
	AllowedTools: []string{"read_file", "list_directory"}, // expose only these
	BlockedTools: []string{"delete_file"},                 // hide these
	Strict:       true,                                    // normalize schemas to OpenAI strict mode
	ClientName:   "my-app", ClientVersion: "1.2.0",        // reported to the server

	CacheToolsList: true,            // cache list_tools across turns
	ToolNamePrefix: "github_",       // avoid name clashes between servers
	ToolFilter: func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, name string) bool {
		return rc.Context != nil // expose tools only in some run contexts
	},
	RequireApproval: func(name string) bool { return name == "delete_file" }, // HITL
}
```

- **Tool filtering**: `AllowedTools` whitelists, `BlockedTools` blacklists (blocked wins). `ToolFilter` adds a dynamic, per-call decision on top — it sees the run context and the tool's original name, and runs on every `ListTools` even when the list is cached.
- **`CacheToolsList`**: caches the server's tool list after the first fetch so a multi-turn run does not re-issue `list_tools` each turn. Call `server.InvalidateToolsCache()` when the server's tools may have changed. Filters still run on every call.
- **`ToolNamePrefix`**: prepends a prefix to each exposed tool name so several servers can expose same-named tools without colliding; the server is still called with the original name.
- **`RequireApproval`**: marks matching tools as needing human approval, routing them through the [HITL](human_in_the_loop.md) flow like any `NeedsApproval` function tool.
- **Strict**: rewrites each tool's input schema to the strict subset; if a server's schema cannot be made strict, the original schema is used and strict mode is disabled for that tool (never half-converted).

## Prompts and resources

A `Server` also exposes the MCP prompt and resource endpoints directly (they are not agent tools — call them yourself, e.g. to seed instructions from a server-managed prompt):

```go
prompts, _ := server.ListPrompts(ctx, nil)
p, _ := server.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: "code_review", Arguments: map[string]string{"lang": "go"}})
// p.Messages -> turn into agent instructions or input

resources, _ := server.ListResources(ctx, nil)
r, _ := server.ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "file:///README.md"})
// r.Contents -> inject as context
```

## Behavior

- Tool call results prefer the server's `structuredContent` (JSON), then a single text block verbatim; multiple or non-text content blocks are JSON-encoded so nothing is dropped.
- Image results are passed through natively: when a result carries image content (an `image` block, or an embedded resource with an `image/*` MIME type) it becomes a `function_call_output` content list so the model receives real image input — text blocks stay text, other blocks are JSON-encoded into a text part. This builds on [structured tool output](tools.md#structured--multimodal-output).
- A tool call that fails — including results flagged `isError` — is fed back to the model as the tool output so it can recover, like any function tool failure. Set the produced tool's `FailureErrorFunction` to nil if you want failures to abort the run (advanced).
- `Close()` shuts the session down; it is safe to call once finished with the server.

Not modeled: provider-hosted MCP (OpenAI's server-side MCP tool), per the SDK's no-hosted-tools stance.
