# Model context protocol (MCP)

[MCP](https://modelcontextprotocol.io/) is an open protocol for exposing tools (and other capabilities) to LLM applications. The `mcp` package connects an agent to MCP servers over the official [Go SDK](https://github.com/modelcontextprotocol/go-sdk): each server tool becomes a function tool the model can call.

`mcp` is its own Go module (it carries the go-sdk and its transitive closure, [decisions §5.7](../explanation/decisions.md#57-a-submodule-exists-only-to-keep-a-heavy-dependency-out-of-the-core)). The import path is unchanged; add it beside the core:

```bash
go get github.com/zzir/agents-go/mcp
```

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
| `mcp.NewWithTransport(ctx, name, transport, opts)` | Anything implementing the go-sdk `Transport` (e.g. in-memory for tests, or the legacy SSE transport) |

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
	RequireApproval: mcp.ApproveTools("delete_file"), // HITL for these tools
	OAuthHandler: authHandler, // OAuth 2.1 authorization (streamable HTTP only)

	MaxRetryAttempts: 3,              // retry list_tools/call_tool failures (-1 = infinite, 0 = off)
	RetryBackoffBase: time.Second,    // base delay for exponential backoff
}
```

- **Tool filtering**: `AllowedTools` whitelists, `BlockedTools` blacklists (blocked wins). `ToolFilter` adds a dynamic, per-call decision on top — it sees the run context and the tool's original name, and runs on every `ListTools` even when the list is cached.
- **`CacheToolsList`**: caches the server's tool list after the first fetch so a multi-turn run does not re-issue `list_tools` each turn. The cache is invalidated automatically when the server sends a `tools/list_changed` notification; call `server.InvalidateToolsCache()` manually only for servers that change tools without notifying. Filters still run on every call.
- **`ToolNamePrefix`**: prepends a prefix to each exposed tool name so several servers can expose same-named tools without colliding; the server is still called with the original name.
- **`RequireApproval`**: decides per call whether a tool needs human approval, routing it through the [HITL](human_in_the_loop.md) flow like any `NeedsApproval` function tool. It receives the run context, the current agent (captured per `ListTools` call, so the predicate names the agent whose turn it is) and the tool's original name. For the common static list, `mcp.ApproveTools("a", "b")` builds the predicate.
- **`MaxRetryAttempts` / `RetryBackoffBase`**: retry a failed `list_tools` or `call_tool` request with exponential backoff (`RetryBackoffBase * 2^(attempt-1)`, capped at 30s and jittered into `[d/2, d]`). `0` (default) disables retries, `-1` retries indefinitely; `RetryBackoffBase` defaults to one second when retries are enabled. Only transport failures are retried — a JSON-RPC error the server answered with (unknown method, invalid params, …) and a call made after `Close` surface on the first attempt, since another try earns the same answer.
- **Strict**: rewrites each tool's input schema to the strict subset; if a server's schema cannot be made strict, the original schema is used and strict mode is disabled for that tool (never half-converted).
- **`OAuthHandler`**: passes a `go-sdk/auth.OAuthHandler` to the streamable HTTP transport for OAuth 2.1 authorization (authorization code + PKCE, token refresh, dynamic client registration). Ignored for stdio transports. See the [OAuth section](#oauth) below.

Two more behaviors are automatic:

- **Required-argument validation**: before each `call_tool`, the client checks the model's arguments against the tool's schema `required` list. A missing key is a `*agents.UserError` raised *before* the server is touched. Because MCP-bridged tools carry `DefaultToolErrorFunction`, that error is fed back to the model as the tool output so it can retry, rather than aborting the run.
- **Description fallback**: a tool with an empty `description` is shown to the model using its display `title`, then its `annotations.title`, so it is never presented without a description.

## OAuth

The `mcp` package supports OAuth 2.1 for streamable HTTP servers via the go-sdk's `auth` package. Set `Options.OAuthHandler` to an `auth.OAuthHandler` implementation — the built-in `auth.NewAuthorizationCodeHandler` covers the standard authorization code + PKCE flow with optional dynamic client registration:

```go
import "github.com/modelcontextprotocol/go-sdk/auth"

handler, _ := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
	RedirectURL:              "http://localhost:3142",
	AuthorizationCodeFetcher: myFetcher, // opens browser, waits for redirect
})

srv, _ := mcp.NewStreamableHTTPServer(ctx, "my-server", endpoint, mcp.Options{
	OAuthHandler: handler,
})
```

The `agents-server` web UI handles this automatically: configure a server with **Authentication → OAuth**, and the Connect button will open an authorization popup when needed. An authorization server that doesn't support dynamic client registration rejects that default flow with `no configured client registration methods are supported by the authorization server` — register an OAuth client with the provider, set its redirect URI to `<base-url>/api/v1/mcp-servers/oauth/callback`, and fill in **Client ID / Client secret**. The **Scopes** field only feeds dynamic registration; with a pre-registered client the requested scopes come from the server's own metadata.

Changing the endpoint, auth mode or Client ID drops the stored authorization — the grant was minted for the previous identity — so the next connect prompts again.

## Prompts and resources

Protocol surface this package does not adapt — prompts, resources, and whatever the go-sdk grows next — is reached through the underlying client session (they are not agent tools — call them yourself, e.g. to seed instructions from a server-managed prompt):

```go
prompts, _ := server.Session().ListPrompts(ctx, nil)
p, _ := server.Session().GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: "code_review", Arguments: map[string]string{"lang": "go"}})
// p.Messages -> turn into agent instructions or input

resources, _ := server.Session().ListResources(ctx, nil)
r, _ := server.Session().ReadResource(ctx, &mcpsdk.ReadResourceParams{URI: "file:///README.md"})
// r.Contents -> inject as context
```

## Behavior

- Tool call results reach the model as content parts, one per content block: text stays text, image content (an `image` block, or an embedded resource with an `image/*` MIME type) becomes real image input, and everything else is JSON-encoded into a text part. A lone text block still collapses to a plain string. This builds on [structured tool output](tools.md#structured--multimodal-output).
- `Options.UseStructuredContent` instead uses the server's `structuredContent` exclusively, as a single JSON text part, falling back to the content blocks when it is empty. It is off by default because most servers duplicate that data in the blocks.
- A tool call that fails — including results flagged `isError` — is fed back to the model as the tool output so it can recover, like any function tool failure. Set the produced tool's `FailureErrorFunction` to nil if you want failures to abort the run (advanced).
- **A caller's cancellation does not cancel the request.** A connection is shared by everyone using that server, and the streamable HTTP transport fails the whole connection — permanently, for every user of it — when a request it is carrying is cancelled mid-flight. So requests ride the connection's own context: cancelling a run returns from `ListTools`/`CallTool` at once with `context.Canceled`, and the request it was waiting on finishes in the background with its answer dropped. One caller's cancellation costs one in-flight request, not the connection.
- **`Options.Redial` makes a connection self-healing.** Nothing in the go-sdk reconnects, so without it the first death — a server restart, a dropped idle socket, somebody else's cancelled request — is permanent, and every agent configured with that server answers "client is closing" until a person reconnects it by hand. Set `Redial` to a function that rebuilds the transport (a new `CommandTransport` with an unstarted command, a streamable transport with the same endpoint, headers and OAuth handler) and the session is replaced in place, so every holder recovers rather than only the runs that start afterwards. A dying connection is noticed as it dies, not when the next caller trips over it; `tools/list` is re-issued on the fresh session, while a failed tool CALL is reported rather than repeated — a dead connection cannot say whether the server ran it before the line dropped. Healing is throttled, so a server that accepts and immediately drops connections cannot become a dial loop. `agents-server` wires this for its streamable HTTP servers from the stored config.
- `Close()` shuts the session down; it is safe to call once finished with the server. It also ends the connection's context, so requests still in flight — including ones their caller has already abandoned — unwind with it.

Not modeled: provider-hosted MCP (OpenAI's server-side MCP tool), per the SDK's no-hosted-tools stance.
