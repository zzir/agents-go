# Running agents

Run agents with one of three entry points:

- `agents.Run(ctx, agent, input, opts)` — runs the loop to completion, returns a `*RunResult`
- `agents.RunStreamed(ctx, agent, input, opts)` — same loop, but streams events as they happen ([Streaming](streaming.md))
- `agents.ResumeRun(ctx, state, opts)` — continues a run paused for tool approval ([Human-in-the-loop](human_in_the_loop.md))

`input` is either a `string` (treated as a user message) or a `[]agents.TResponseInputItem` — the OpenAI Responses API item list, exactly as in Python.

```go
res, err := agents.Run(ctx, agent, "Write a haiku about recursion.", agents.RunOptions{
	ModelProvider: provider,
})
```

## The agent loop

`Run` executes the same loop as the Python SDK:

1. Call the model for the current agent with the conversation so far.
2. If the model produced a final output (a message with no pending tool calls, matching the agent's output type), the loop ends.
3. If the model requested a handoff, switch the current agent and loop.
4. Otherwise execute the tool calls (concurrently), append their results, and loop.

If the number of turns exceeds the budget, the run fails with `*agents.MaxTurnsError`.

## Run options

```go
type RunOptions struct {
	MaxTurns              int              // turn budget; 0 means DefaultMaxTurns (10)
	Context               any              // your app data, threaded through tools/guardrails/hooks
	RunContext            *RunContext      // use an existing run context instead
	ModelProvider         ModelProvider    // resolves agent model names (required unless ModelImpl/Model set)
	Model                 Model            // run-level model override for every agent
	ModelSettings         *ModelSettings   // run-level settings merged over each agent's own
	Hooks                 RunHooks         // run-scoped lifecycle callbacks
	Session               Session          // conversation persistence (docs: Sessions)
	Tracer                *tracing.Tracer  // opt-in tracing (docs: Tracing)
	UsePreviousResponseID bool             // server-managed conversation state (below)
}
```

## Conversations / chat threads

Each `Run` is one logical turn of a conversation. To carry history across runs you can:

1. **Use a [Session](sessions.md)** — history is loaded before the run and saved after it:

   ```go
   sess := agents.NewInMemorySession()
   agents.Run(ctx, agent, "What city is the Golden Gate Bridge in?", agents.RunOptions{Session: sess, ModelProvider: p})
   agents.Run(ctx, agent, "What state is it in?", agents.RunOptions{Session: sess, ModelProvider: p})
   ```

2. **Thread items manually** — build the next input from the previous result:

   ```go
   res1, _ := agents.Run(ctx, agent, "What city is the Golden Gate Bridge in?", opts)
   input := append(res1.Input, mustInputItems(res1.NewItems)...) // via item.ToInputItem()
   input = append(input, agents.InputItemsFromText("What state is it in?")...)
   res2, _ := agents.Run(ctx, agent, input, opts)
   ```

3. **Let the server keep state** — set `UsePreviousResponseID: true` and the runner chains calls through the Responses API's `previous_response_id`, sending only new items each turn. Requires stored responses (the default; do not set `ModelSettings.Store` to false).

## Cancellation and deadlines

The `context.Context` you pass governs the whole run: cancel it to abort between turns, mid-stream, and inside tool calls (tools receive the same context). This replaces Python's `asyncio` cancellation.

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
defer cancel()
res, err := agents.Run(ctx, agent, input, opts)
```

Per-tool timeouts are a [tool-level setting](tools.md#timeouts).

## Run-scoped hooks

`RunOptions.Hooks` receives callbacks across every agent in the run. Embed `BaseRunHooks` and override what you need; returning an error aborts the run.

```go
type auditHooks struct{ agents.BaseRunHooks }

func (auditHooks) OnHandoff(ctx context.Context, rc *agents.RunContext, from, to *agents.Agent) error {
	log.Printf("handoff %s -> %s", from.Name, to.Name)
	return nil
}
```

Callbacks: `OnAgentStart`, `OnAgentEnd`, `OnHandoff`, `OnToolStart`, `OnToolEnd`. Tool callbacks may fire concurrently (tools run in parallel), so hooks must be goroutine-safe.

## Errors

All failures come back as Go errors. The SDK's typed errors embed `agents.AgentsError` and can be matched with `errors.As`:

| Error | Meaning |
|---|---|
| `*MaxTurnsError` | Turn budget exhausted (`errors.Is(err, agents.ErrMaxTurns)` also works) |
| `*ModelBehaviorError` | The model did something invalid (unknown tool, malformed structured output, truncated stream) |
| `*ModelRefusalError` | The model refused to respond; carries the refusal text |
| `*UserError` | You used the SDK incorrectly (e.g. no model provider, invalid output schema) |
| `*ToolTimeoutError` | A tool exceeded its `FunctionTool.Timeout` |
| `*InputGuardrailTripwireError` / `*OutputGuardrailTripwireError` / `*ToolGuardrailTripwireError` | A guardrail tripped |

Every SDK error carries `Details *RunErrorDetails` (input, items generated so far, raw responses, last agent, usage) so you can inspect partial progress — see [Results](results.md#errors).
