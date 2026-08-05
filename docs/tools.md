# Tools

Tools let agents take actions. They come from three places:

- **Function tools**: any typed Go function, with the JSON schema reflected from its argument struct
- **Agents as tools**: a whole agent exposed as a callable tool ([Agent orchestration](multi_agent.md))
- **MCP tools**: tools served by a Model Context Protocol server ([MCP](mcp.md))

All three end up as the same thing — a locally executed `*FunctionTool`. It is a
**struct, not an interface**, so there is exactly one execution path to reason
about, and a tool cannot quietly mean "the provider runs this".

That is also why hosted OpenAI tools (web search, file search, code
interpreter, computer use) are **not modeled, and will not be** — a hosted tool
binds the agent to one backend. Where the capability matters, the SDK gives you
a local equivalent you own: `apply_patch` and shell access run through the
[Sandbox](sandbox.md) abstraction rather than a provider's. See
[spec.md §1.2](spec.md#12-non-goals) for the decision and
[Differences from Python](migration_from_python.md) for the full list.

## Function tools

`NewFunctionTool[A, R]` turns a Go function into a tool. The argument type `A` (a struct) is reflected into a strict JSON schema; the result `R` is returned to the model (serialized to JSON unless it is already a string).

```go
type queryArgs struct {
	SQL   string `json:"sql" jsonschema:"the SQL query to run"`
	Limit int    `json:"limit" jsonschema:"max rows to return"`
}

runQuery := agents.NewFunctionTool("run_query", "Run a read-only SQL query.",
	func(ctx context.Context, tc *agents.ToolContext, args queryArgs) ([]map[string]any, error) {
		return db.Query(ctx, args.SQL, args.Limit)
	})

agent.Tools = []*agents.FunctionTool{runQuery}
```

- The `jsonschema:"..."` struct tag is the parameter description shown to the model.
- The `ctx` is the run's context (cancellation propagates into tools).
- `tc *ToolContext` carries the [run context](context.md) plus call metadata: `ToolName`, `ToolCallID`, `ToolArguments`, the `Agent` whose tool is running, and `ToolCall` (the raw model-emitted function-call item). To observe or gate the call from outside the tool, use tool-stage [guardrails](guardrails.md) — they bracket execution with the same call identity in their payload.
- Tools requested in the same model turn run **concurrently**; share state through the context value only if it is goroutine-safe.

The schema comes from compile-time generics over the argument struct and its tags, so what the model is shown and what the function decodes cannot drift apart.

### Strict mode

Strict schema mode is on by default and the reflected schema is rewritten to the strict subset OpenAI requires (`additionalProperties:false`, all properties required, …). Chain `NonStrict()` when the model should be allowed to omit fields whose json tag carries `,omitempty` — it relaxes the advertised schema and the local argument validation together:

```go
t := agents.NewFunctionTool("lookup", "…", fn).NonStrict()
```

`NewFunctionTool` panics if the argument type cannot be reflected into a schema (not a struct, or a field no schema can express) — a deterministic programmer error, surfaced at construction like `regexp.MustCompile`. For schemas that are runtime data, `NewRawFunctionTool` returns an error instead.

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

`*FunctionTool` is a struct, so a variant of a tool you did not construct — one
returned by `agent.AsTool(...)`, by an MCP server, or by a library — is a copy
with the fields you want changed:

```go
gated := *tool                 // copy; the schema and validator are shared but never mutated
gated.NeedsApproval = true
gated.Timeout = 30 * time.Second
gated.Guardrails = append(gated.Guardrails, myGuardrail)  // append: never drop the tool's own
agent.Tools = append(agent.Tools, &gated)
```

Two rules follow from the fields rather than from a framework:

- **Append to `Guardrails`, do not assign.** Replacing the slice disarms the
  checks the tool declared for itself.
- **Capture a hook before overwriting it** when your version should compose with
  the tool's own answer rather than replace it:

  ```go
  inner := tool.IsEnabled
  gated.IsEnabled = func(ctx context.Context, rc *agents.RunContext, a *agents.Agent) (bool, error) {
      if !unlocked() {
          return false, nil
      }
      if inner != nil {
          return inner(ctx, rc, a)
      }
      return true, nil
  }
  ```

The runner reads these fields directly. There is nothing to unwrap and no
capability lookup to get wrong: a tool's timeout is `tool.Timeout`, and a
copy that did not touch it still has it.

### Progressive disclosure

`FunctionTool.Deferred` withholds a tool from the model until another tool's
result names it:

```go
readAccount.Deferred = true             // hidden until disclosed

agent.Tools = []*agents.FunctionTool{
	authenticate,                       // always available
	readAccount,
}

// inside authenticate:
r := agents.TextResult("signed in")
r.AddedTools = []string{"read_account"}
return r, nil
```

An agent offered forty tools chooses worse than one offered four, and most of
those forty only matter after something else has happened. A tool announcing
what it unlocks says that directly, where a static list cannot.

Marking the *tool* is the opt-in rather than a run-level switch, because the
interesting question is which tools wait — a run where everything is deferred
has no way to disclose anything.

Disclosure is **cumulative** for the rest of the run (withdrawing a tool after
one use would surprise a model that had just been told it existed) and survives
an [approval pause](human_in_the_loop.md), so a resumed run does not re-hide it.
It does not override `IsEnabled`: disclosure opens a door, it does not force
one. Naming a tool that does not exist is ignored — a tool should not be able to
fail a run by mentioning something.

### Streaming partial results

A tool that runs for a while can push progress to a streamed run's consumer:

```go
tool := agents.NewFunctionTool("build", "Build the project.",
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

**Progress is not the answer.** It never reaches the model — the tool's return
value does. `Emit` is a no-op on a blocking run and after the tool returns, so
a tool never has to ask which kind of run it is in, and a goroutine it left
behind cannot keep reporting on a finished call. It is safe from any goroutine.

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

renderChart := agents.NewFunctionTool("render_chart", "Render a chart as an image.",
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

### Returning more than a value: `ToolResult`

A tool that needs to say more than "here is the answer" returns a `ToolResult`
instead of a plain value:

```go
agents.NewFunctionTool("query_orders", "…",
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
| `Usage` | Tokens the tool spent on model calls **of its own** (an agent-as-tool's nested run, a summarization step) |
| `Terminate` | Ask the run to stop after this batch |
| `IsError` | Render as a failure. The content still goes to the model, so it can recover |

Everything else keeps working: a tool returning a `string`, a struct, or a
`[]ToolOutputContent` is wrapped automatically, so `return "sunny", nil` is
still the shortest correct tool.

**`Details` must survive a JSON round-trip.** A value that cannot (NaN/Inf
floats, channels, cycles) fails the run *while the tool call is still
identifiable*, rather than at persistence time long after. An empty map
normalizes to nil.

**`Terminate` needs unanimity.** The run stops only when every tool in the batch
asks. One tool wanting to stop while another is still working is not a decision
the SDK can make for them, and stopping anyway would throw away the other's
result.

This replaces `CustomDataExtractor`, which ran a second pass over the finished
call to produce UI data, and the consumer-side patching that attached it
afterwards. The tool already knew all of it at the moment it returned.

### Hand-built tools

`FunctionTool` is an exported struct, so advanced callers can build one directly with a custom `ParamsJSONSchema` and raw-JSON `OnInvoke` (which returns a `ToolResult` — use `agents.TextResult` for the common case):

```go
t := &agents.FunctionTool{
	Name:             "echo",
	Description:      "Echo the arguments back.",
	ParamsJSONSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}, "additionalProperties": false},
	Strict:           true,
	FailureErrorFunction: agents.DefaultToolErrorFunction,
	OnInvoke: func(ctx context.Context, tc *agents.ToolContext, argsJSON string) (any, error) {
		return argsJSON, nil
	},
}
```

## Sandbox code tools

`sandbox.CodeTool` wraps an isolated execution backend (local, Docker) as a "run this code" tool — see [Sandbox agents](sandbox.md).

## Web search (Brave)

`tools/bravesearch` is a ready-made function tool that searches the web via the [Brave Search API](https://api-dashboard.search.brave.com/api-reference/web/search/get). It is a plain, provider-agnostic function tool — the SDK calls Brave's REST API from Go and returns formatted results — so it works with any model backend (the SDK does not use provider-hosted search tools).

```go
import "github.com/zzir/agents-go/tools/bravesearch"

search, err := bravesearch.New(bravesearch.Options{
    // APIKey defaults to the BRAVE_API_KEY environment variable.
    Count: 5, // results to request (1-20)
})
if err != nil {
    log.Fatal(err)
}

agent := &agents.Agent{
    Name:  "research-bot",
    Model: "gpt-4o",
    Tools: []*agents.FunctionTool{search},
}
```

The model controls only the `query`; `Count`, `Country`, `SearchLang`, `SafeSearch` and `Freshness` are fixed by `Options`. A runnable example lives in `examples/bravesearch`.

## File editing

File editing is a **sandbox** capability: `apply_patch` (Codex-style multi-file
patches) edits through the `Sandbox` abstraction, so it targets the same
filesystem `exec_command` and the file tools use — a local dir, a bind-mounted
container, or a remote host over SFTP. There is no separate local-path editor
and no hosted OpenAI `apply_patch`.

```go
tools := []*agents.FunctionTool{sandbox.CodeTool(sb, sandbox.CodeToolConfig{})}
tools = append(tools, sandbox.FileTools(sb, sandbox.FileToolConfig{})...)   // read_file, write_file, list_files
tools = append(tools, sandbox.ApplyPatchTool(sb, sandbox.FileToolConfig{})) // apply_patch
```

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
