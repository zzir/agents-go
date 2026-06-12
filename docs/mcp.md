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
}
```

- **Tool filtering**: `AllowedTools` whitelists, `BlockedTools` blacklists (blocked wins). The Go SDK supports static filters only — Python's context-dependent callable filters have no counterpart yet.
- **Strict**: rewrites each tool's input schema to the strict subset; if a server's schema cannot be made strict, the original schema is used and strict mode is disabled for that tool (never half-converted).

## Behavior

- Tool call results prefer the server's `structuredContent` (JSON), then a single text block verbatim; multiple or non-text content blocks are JSON-encoded so nothing is dropped.
- A tool call that fails — including results flagged `isError` — is fed back to the model as the tool output so it can recover, like any function tool failure. Set the produced tool's `FailureErrorFunction` to nil if you want failures to abort the run (advanced).
- `Close()` shuts the session down; it is safe to call once finished with the server.

Prompts, resources, and tool-list caching are not implemented yet — see [differences](python_differences.md).
