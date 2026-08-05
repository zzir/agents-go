# Context management

"Context" means two distinct things, and the Go SDK keeps them separate by design:

1. **Local context your code needs** — dependencies, the current user, loggers… This never reaches the LLM.
2. **Conversation context the LLM sees** — the input item history. This is what [Sessions](sessions.md), tool results and instructions shape.

## Local context: `RunContext`

Two concerns that are often conflated stay separate here:

- **Cancellation/deadlines** ride the standard `context.Context` passed to `Run` (and through to every tool, guardrail and hook).
- **Your data** rides `RunContext.Context`, an `any` value you supply via `RunOptions.Context`. The SDK never inspects it.

```go
type AppContext struct {
	UserID string
	DB     *sql.DB
}

res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{
	Context: &AppContext{UserID: "u_123", DB: db},
	Model: agents.ModelOptions{Provider: provider},
})
```

Every tool, guardrail, hook and dynamic-instructions function receives the same `*agents.RunContext`; type-assert your value back:

```go
tool := agents.NewFunctionTool("whoami", "Return the current user.",
	func(ctx context.Context, tc *agents.ToolContext, _ struct{}) (string, error) {
		app := tc.RunContext.Context.(*AppContext)
		return app.UserID, nil
	})
```

`RunContext` also carries run-scoped state the SDK maintains for you:

| Field | Meaning |
|---|---|
| `Context` | Your value, verbatim |
| `Usage` | Token usage accumulated so far ([Usage](usage.md)) |
| `Approvals` | Recorded human-in-the-loop decisions ([Human-in-the-loop](human_in_the_loop.md)) |

`ToolContext` embeds `*RunContext` and adds the call's `ToolName`, `ToolCallID` and raw `ToolArguments`.

> Tools run concurrently within a turn. If tools mutate your context value, make it goroutine-safe.

## Agent/LLM context

The LLM only sees the conversation history. To expose data to it:

1. **Instructions** — static or computed from the run context ([dynamic instructions](agents.md#dynamic-instructions)). Good for always-relevant facts (user name, current date).
2. **Input** — append data to the input items when calling `Run`.
3. **Tools** — let the model fetch data on demand via [function tools](tools.md).
4. **Retrieval / MCP** — ground answers in external documents or services ([MCP](mcp.md)).
