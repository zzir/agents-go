# Tools

Tools let agents take actions. They come from three places:

- **Function tools**: any typed Go function, with the JSON schema reflected from its argument struct
- **Agents as tools**: a whole agent exposed as a callable tool ([Agent orchestration](multi_agent.md))
- **MCP tools**: tools served by a Model Context Protocol server ([MCP](mcp.md))

All three end up as the same thing — a locally executed `*Tool` **struct**, so
there is one execution path and nothing a provider-hosted tool could implement.
Hosted OpenAI tools (web search, file search, code interpreter, computer use)
are therefore not modeled ([scope §1.2](../explanation/scope.md#12-non-goals));
the local equivalents you own — `apply_patch` and shell access — run through the
[Sandbox](sandbox.md) abstraction.

## Function tools

`NewTool[A, R]` turns a Go function into a tool. The argument type `A` (a struct) is reflected into a strict JSON schema; the result `R` is returned to the model (serialized to JSON unless it is already a string).

```go
type queryArgs struct {
	SQL   string `json:"sql" jsonschema:"the SQL query to run"`
	Limit int    `json:"limit" jsonschema:"max rows to return"`
}

runQuery := agents.NewTool("run_query", "Run a read-only SQL query.",
	func(ctx context.Context, tc *agents.ToolContext, args queryArgs) ([]map[string]any, error) {
		return db.Query(ctx, args.SQL, args.Limit)
	})

agent.Tools = []*agents.Tool{runQuery}
```

- The `jsonschema:"..."` struct tag is the parameter description shown to the model.
- The `ctx` is the run's context (cancellation propagates into tools).
- `tc *ToolContext` carries the [run context](running_agents.md#local-context) plus call metadata: `ToolName`, `ToolCallID`, `ToolArguments`, the `Agent` whose tool is running, and `ToolCall` (the raw model-emitted function-call item). To observe or gate the call from outside the tool, use tool-stage [guardrails](guardrails.md) — they bracket execution with the same call identity in their payload.
- Tools requested in the same model turn run **concurrently**; share state through the context value only if it is goroutine-safe.

The schema comes from compile-time generics over the argument struct and its tags, so what the model is shown and what the function decodes cannot drift apart.

### Strict mode

Strict schema mode is on by default and the reflected schema is rewritten to the strict subset OpenAI requires (`additionalProperties:false`, all properties required, …). Chain `NonStrict()` when the model should be allowed to omit fields whose json tag carries `,omitempty` — it relaxes the advertised schema and the local argument validation together:

```go
t := agents.NewTool("lookup", "…", fn).NonStrict()
```

`NewTool` panics if the argument type cannot be reflected into a strict schema (not a struct, a field no schema can express, or a shape strict mode cannot express at all — an `any`/`interface{}` field, a map with arbitrary keys) — a deterministic programmer error, surfaced at construction like `regexp.MustCompile`. For schemas that are runtime data, `NewRawTool` returns an error instead.

That last shape is the one `NonStrict()` cannot rescue: the strict schema is generated during construction, so the panic happens before there is a tool to relax. Build those with `NewToolNonStrict`, which is `NewTool` without the strict rewrite — arguments are still validated against the schema the model was shown:

```go
save := agents.NewToolNonStrict("save", "Store an arbitrary JSON payload.", saveFn)
```

### Error handling

By default a tool error is fed back to the model as the tool output so it can recover (`DefaultToolErrorFunction`). Customize the message, or make errors fatal:

```go
t.FailureErrorFunction = func(ctx context.Context, tc *agents.ToolContext, err error) string {
	return "lookup failed, try a different spelling"
}
t.FailureErrorFunction = nil // a tool error now aborts the whole run
```

### Timeouts

`Timeout` bounds one invocation; on expiry the call fails with `*agents.ToolTimeoutError` immediately (fed back to the model via `FailureErrorFunction` when set, fatal otherwise). The deadline is enforced by the runner rather than by the tool's cooperation: a tool that ignores its context cannot stall the run — its goroutine keeps running in the background until it returns on its own and its late result is discarded. Tools should still honor `ctx` cancellation to release resources promptly:

```go
t.Timeout = 30 * time.Second
```

### Conditionally enabling tools

`IsEnabled` decides per run whether the tool is offered to the model:

```go
t.IsEnabled = func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) (bool, error) {
	return rc.Context.(*MyAppContext).IsAdmin, nil
}
```

### Approval (human-in-the-loop)

`NeedsApproval` (or per-call `NeedsApprovalFunc`) pauses the run before the tool executes, surfacing an interruption you approve or reject — see [Human-in-the-loop](human_in_the_loop.md). The per-call predicate is `func(ctx context.Context, rc *agents.RunContext, argsJSON string, callID string) (bool, error)`, so a decision can turn on the raw arguments and the model-assigned call id.

### Tool guardrails

Tools can carry their own input/output guardrails — see [Guardrails](guardrails.md#placement-decides-scope).

### Adapting a tool you did not build

`*Tool` is a struct, so a variant of a tool you did not construct — one
returned by `agent.AsTool(...)`, by an MCP server, or by a library — is a copy
with the fields you want changed:

```go
gated := *tool                 // copy; the schema and validator are shared but never mutated
gated.NeedsApproval = true
gated.Timeout = 30 * time.Second
gated.Guardrails = append(gated.Guardrails, myGuardrail)  // append: never drop the tool's own
agent.Tools = append(agent.Tools, &gated)
```

Append to `Guardrails` rather than assign, and capture a hook
(`inner := tool.IsEnabled`) before overwriting it when yours should compose
with the tool's own answer ([spec §2.7c](../reference/spec.md#27c-tool-capabilities-are-fields)).

### Progressive disclosure

`Tool.Deferred` withholds a tool from the model until another tool's
result names it:

```go
readAccount.Deferred = true             // hidden until disclosed

agent.Tools = []*agents.Tool{
	authenticate,                       // always available
	readAccount,
}

// inside authenticate:
r := agents.TextResult("signed in")
r.AddedTools = []string{"read_account"}
return r, nil
```

Disclosure is cumulative for the run, survives an [approval pause](human_in_the_loop.md), does not override `IsEnabled`, and naming an unknown tool is ignored ([spec §2.7i](../reference/spec.md#27i-progressive-tool-disclosure)).

### Streaming partial results

A tool that runs for a while can push progress to a streamed run's consumer:

```go
tool := agents.NewTool("build", "Build the project.",
	func(ctx context.Context, tc *agents.ToolContext, a buildArgs) (string, error) {
		for _, step := range steps {
			tc.Emit(agents.TextResult(step.Name).WithDisplay("terminal"))
			…
		}
		return summary, nil
	})
```

The consumer receives a `*agents.ToolProgressEvent` carrying the tool name, call
id and the partial `ToolResult`:

```go
for ev, err := range stream {
	if p, ok := ev.(*agents.ToolProgressEvent); ok {
		fmt.Printf("[%s] %s\n", p.ToolName, p.Result.ModelOutput())
	}
}
```

Progress never reaches the model — the return value does — and `Emit` is a safe no-op on a blocking run and after the tool returns ([spec §2.7g](../reference/spec.md#27g-tool-progress)).

Two built-ins already use it: `sandbox.CodeTool` streams stdout as the command
runs (on backends implementing `ExecStreamer`), and an
[agent-as-tool](multi_agent.md) forwards the nested agent's messages, so a
sub-agent's work is visible without wiring `OnStream`.

### Structured / multimodal output

By default a tool's return value goes back to the model as text (JSON for non-string values). To hand the model **native image or file input** instead, return a `ToolOutputContent` — or a `[]ToolOutputContent` for several parts — which becomes a `function_call_output` content list:

```go
type chartArgs struct {
	Metric string `json:"metric" jsonschema:"which metric to chart"`
}

renderChart := agents.NewTool("render_chart", "Render a chart as an image.",
	func(ctx context.Context, tc *agents.ToolContext, args chartArgs) ([]agents.ToolOutputContent, error) {
		png := plot(args.Metric) // []byte
		return []agents.ToolOutputContent{
			agents.ToolOutputText{Text: "chart for " + args.Metric},
			agents.ToolOutputImageFromBytes("image/png", png),
		}, nil
	})
```

The three content parts mirror the Responses API:

- `ToolOutputText{Text}` — a text part (same as returning the string directly, but combinable with images/files).
- `ToolOutputImage{ImageURL, FileID, Detail}` — native image input; set `ImageURL` (a URL or a base64 `data:` URL — `ToolOutputImageFromBytes(mime, bytes)` builds one) **or** `FileID` (an uploaded file).
- `ToolOutputFile{FileData, FileURL, FileID, Filename}` — native file input (e.g. a PDF).

A runnable example lives in `examples/toolimage`. It is also what lets MCP image results reach the model as real images ([MCP](mcp.md)).

For a UI, the item's `Display().Output` is the same content list as JSON — `[{"type":"input_text","text":"…"},{"type":"input_image","image_url":"data:…"}]` — so a renderer that reads `type` can show the image (or offer the file) instead of printing the payload.

### Returning more than a value: `ToolResult`

A tool that needs to say more than "here is the answer" returns a `ToolResult`
instead of a plain value:

```go
agents.NewTool("query_orders", "…",
	func(ctx context.Context, tc *agents.ToolContext, args Query) (agents.ToolResult, error) {
		rows := query(args)
		return agents.TextResult(summarize(rows)).
			WithDisplay("table").
			WithDetails(map[string]any{"row_count": len(rows)}), nil
	})
```

| Field | What it is for |
|---|---|
| `Content` | What the model sees — text, images, files |
| `Details` | Structured data for the UI and logs. **Never reaches the model.** Lands on `Display().Extra` |
| `Display` | The renderer you would like: `"diff"`, `"terminal"`, `"table"`, `"json"`, `"markdown"`. A hint — an unknown name falls back to text |
| `Title` / `Summary` | A card heading when the tool name is not it, and a one-line account of what happened (`WithTitle`/`WithSummary`); overrides a consumer may ignore, never reaching the model |

A tool returning a `string`, a struct, or a `[]ToolOutputContent` is wrapped automatically, so `return "sunny", nil` is still the shortest correct tool. `Details` must survive a JSON round-trip or the call fails while it is still identifiable; `Terminate` stops the run only when every tool in the batch asks; `IsError` renders a failure whose content still reaches the model; `Usage` is the tool's own model spend ([spec §2.7b](../reference/spec.md#27b-tool-results)).

### Hand-built tools

`Tool` is an exported struct, so advanced callers can build one directly with a custom `ParamsJSONSchema` and raw-JSON `OnInvoke` (which returns a `ToolResult` — use `agents.TextResult` for the common case):

```go
t := &agents.Tool{
	Name:             "echo",
	Description:      "Echo the arguments back.",
	ParamsJSONSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}, "additionalProperties": false},
	Strict:           true,
	FailureErrorFunction: agents.DefaultToolErrorFunction,
	OnInvoke: func(ctx context.Context, tc *agents.ToolContext, argsJSON string) (agents.ToolResult, error) {
		return agents.TextResult(argsJSON), nil
	},
}
```

## Sandbox code tools

`sandbox.CodeTool` wraps an isolated execution backend — local, Docker (`sandbox/docker`) or an E2B-compatible service (`sandbox/e2b`) — as a "run this code" tool; see [Sandbox agents](sandbox.md).

## Web search

There is no built-in web-search tool: the SDK deliberately does not model
provider-hosted search (scope §3), and the workbench's answer is an MCP server
the operator (or a member) configures ([decisions §5.30](../explanation/decisions.md)). In your own
embedding, a search tool is an ordinary `NewTool` function that calls
whichever API you use.

## File editing

File editing is a **sandbox** capability: `apply_patch` (Codex-style multi-file
patches) edits through the `Sandbox` abstraction, so it targets the same
filesystem `exec_command` and the file tools use — a local dir, a container's
bind mount or volume. There is no separate local-path editor and no hosted
OpenAI `apply_patch`. Wiring it up is in [Sandbox agents](sandbox.md#quickstart).

The patch format carries **no line numbers** — a change is located by its
surrounding context lines and an optional `@@` anchor, so the model never
computes offsets:

```
*** Begin Patch
*** Update File: main.go
@@ func main()
 	fmt.Println("start")
-	x := 1
+	x := 2
*** Add File: notes.md
+created
*** Delete File: stale.txt
*** End Patch
```

- Prefix context lines with a space, removals with `-`, additions with `+`; include enough context to locate each change.
- Rename a file by putting `*** Move to: new/path` right after its `*** Update File:` line.
- **Atomic**: new content is computed entirely in memory first, so a hunk that can't be located changes nothing; if a write fails mid-commit, the already-applied files are rolled back from an in-memory snapshot.

Editing needs a sandbox with a working directory (`ReadFile`/`WriteFile` fail with `ErrNoWorkDir` otherwise). A runnable example lives in `examples/sandbox`.
