# Running agents

Run agents with one of three entry points:

- `agents.Run(ctx, agent, input, opts)` — runs the loop to completion, returns a `*RunResult`
- `agents.Run(ctx, agent, input, opts)` — the same loop as a stream you range, plus a control handle ([Streaming](streaming.md))
- `agents.ResumeRun(ctx, state, opts)` — continues a run paused for tool approval ([Human-in-the-loop](human_in_the_loop.md))

`input` is either a `string` (treated as a user message) or a `[]agents.TResponseInputItem` — the OpenAI Responses API item list, exactly as in Python.

```go
res, err := agents.RunSync(ctx, agent, "Write a haiku about recursion.", agents.RunOptions{
	ModelProvider: provider,
})
```

## The agent loop

`Run` executes the same loop as the Python SDK:

1. Call the model for the current agent with the conversation so far.
2. If the model produced a final output (a message with no pending tool calls, matching the agent's output type), the loop ends.
3. If the model requested a handoff, switch the current agent and loop.
4. Otherwise execute the tool calls (concurrently), append their results, and loop.

If the number of turns exceeds the budget, the run fails with `*agents.MaxTurnsError` — unless a [`MaxTurns` error handler](#error-handlers) recovers it with a fallback final output.

## Run options

```go
type RunOptions struct {
	MaxTurns              int              // turn budget; 0 means DefaultMaxTurns (10)
	Context               any              // your app data, threaded through tools/guardrails/hooks
	RunContext            *RunContext      // use an existing run context instead
	ModelProvider         ModelProvider    // resolves agent model names (required unless ModelImpl/Model set)
	Model                 Model            // run-level model override for every agent
	ModelSettings         *ModelSettings   // run-level settings merged over each agent's own
	CallModelInputFilter  CallModelInputFilter // edit instructions/input just before each model call
	MaxToolConcurrency    int              // cap parallel function tools per turn; 0 = unlimited
	ToolNotFoundBehavior  ToolNotFoundBehavior // unknown tool call: abort (default) or return error to model
	HandoffInputFilter    func(HandoffInputData) HandoffInputData // default filter for handoffs without their own
	ErrorHandlers         RunErrorHandlers // recover from max-turns/refusal/invalid-output failures (below)
	Hooks                 RunHooks         // run-scoped lifecycle callbacks
	Session               Session          // conversation persistence (docs: Sessions)
	SessionInputCallback  SessionInputCallback // combine stored history with new input (docs: Sessions)
	SessionSettings       *SessionSettings // how the run reads the Session, e.g. Limit (docs: Sessions)
	ReasoningItemIDPolicy ReasoningItemIDPolicy // keep (default) or omit reasoning-item ids on replay
	Tracer                *tracing.Tracer  // opt-in tracing (docs: Tracing)
	UsePreviousResponseID bool             // server-managed conversation state (below)
	ConversationID        string           // attach to a server-side OpenAI conversation (below)
}
```

A few control knobs worth calling out:

- **`CallModelInputFilter`** runs just before each model call to edit the system instructions and input items actually sent (e.g. trim tokens, inject context). It does not change what a [session](sessions.md) saves. It does not fire on a HITL-resumed turn.
- **`MaxToolConcurrency`** bounds how many of a turn's function tools run at once (they otherwise all run in parallel) — useful against downstream rate limits.
- **`ToolNotFoundBehavior`** defaults to `ToolNotFoundError` (a hallucinated tool name aborts the run). Set `ToolNotFoundReturnToModel` to instead feed an error back as the tool output so the model can correct itself.
- **`HandoffInputFilter`** applies to any handoff that doesn't set its own `Handoff.InputFilter` — e.g. `agents.NestHandoffHistory(...)` to fold prior history across every handoff ([Handoffs](handoffs.md#nesting-handoff-history)).
- **`SessionInputCallback`** and **`SessionSettings`** tune how a [session](sessions.md#combining-history-with-new-input) is read: the callback decides how stored history is combined with the new input, and `SessionSettings{Limit}` caps how many recent items are loaded. Both are ignored without a `Session`.
- **`ReasoningItemIDPolicy`** controls whether reasoning-item ids survive when run items are converted back into model input on later turns. `ReasoningItemIDPreserve` (the default) keeps them; `ReasoningItemIDOmit` strips them — useful for `store=false` runs whose server-side ids are no longer valid and that rely on encrypted content. The choice is persisted across HITL interruptions in `RunState`.

## Conversations / chat threads

Each `Run` is one logical turn of a conversation. To carry history across runs you can:

1. **Use a [Session](sessions.md)** — history is loaded before the run and saved incrementally as it proceeds (each turn as it completes):

   ```go
   sess := agents.NewInMemorySession()
   agents.Run(ctx, agent, "What city is the Golden Gate Bridge in?", agents.RunOptions{Session: sess, ModelProvider: p})
   agents.Run(ctx, agent, "What state is it in?", agents.RunOptions{Session: sess, ModelProvider: p})
   ```

2. **Thread items manually** — build the next input from the previous result:

   ```go
   res1, _ := agents.RunSync(ctx, agent, "What city is the Golden Gate Bridge in?", opts)
   input := append(res1.Input, mustInputItems(res1.NewItems)...) // via item.ToInputItem()
   input = append(input, agents.InputItemsFromText("What state is it in?")...)
   res2, _ := agents.RunSync(ctx, agent, input, opts)
   ```

3. **Let the server keep state** — two server-managed options, each sending only new items each turn instead of resending history. Neither may be combined with a local `Session` (the run errors if you try):
   - Set `UsePreviousResponseID: true` to chain calls through the Responses API's `previous_response_id`. Requires stored responses (the default; do not set `ModelSettings.Store` to false).
   - Set `ConversationID: "conv_..."` to attach the run to a server-side [OpenAI conversation](https://platform.openai.com/docs/guides/conversation-state) (the Responses `conversation` parameter). Create one with `openai.NewConversationsSession().ConversationID(ctx)`, or use `openai.ConversationsSession` directly as the `Session` for the same effect with local item access.

## Cancellation and deadlines

The `context.Context` you pass governs the whole run: cancel it to abort between turns, mid-stream, and inside tool calls (tools receive the same context). This replaces Python's `asyncio` cancellation.

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
defer cancel()
res, err := agents.RunSync(ctx, agent, input, opts)
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

Callbacks: `OnAgentStart`, `OnAgentEnd`, `OnHandoff`, `OnToolStart`, `OnToolEnd`, `OnLLMStart`, `OnLLMEnd`. `OnToolStart`/`OnToolEnd` receive a `*ToolContext` identifying the specific call (tool, call id, arguments, agent) rather than the run-wide `*RunContext`. `OnLLMStart`/`OnLLMEnd` bracket each model call (the second carries the `*ModelResponse`); they do not fire on a HITL-resumed turn. Tool callbacks may fire concurrently (tools run in parallel), so hooks must be goroutine-safe.

## Errors

All failures come back as Go errors. The SDK's typed errors embed `agents.AgentsError` and can be matched with `errors.As`:

| Error | Meaning |
|---|---|
| `*MaxTurnsError` | Turn budget exhausted (`errors.Is(err, agents.ErrMaxTurns)` also works) |
| `*ModelBehaviorError` | The model did something invalid (unknown tool, malformed structured output, truncated stream) |
| `*ModelRefusalError` | The model refused to respond; carries the refusal text |
| `*UserError` | You used the SDK incorrectly (e.g. no model provider, invalid output schema) |
| `*ToolTimeoutError` | A tool exceeded its `FunctionTool.Timeout` |
| `*GuardrailTripwireError` | A guardrail tripped; `Stage()` says where |

Every SDK error carries `Details *RunErrorDetails` (input, items generated so far, raw responses, last agent, usage) so you can inspect partial progress — see [Results](results.md#errors).

### Error codes

For anything that has to *travel* — an HTTP response, a WebSocket frame, a log
line — match on the code rather than the type. `CodeOf` unwraps `%w` chains, so
it works on whatever `Run` returned regardless of how the run loop wrapped it:

```go
switch agents.CodeOf(err) {
case agents.CodeMaxTurns:          // "max_turns_exceeded"
case agents.CodeGuardrailTripwire: // "guardrail_tripwire"
case agents.CodeToolTimeout:       // "tool_timeout"
case agents.CodeUnknown:           // not an SDK error, or unclassified
}
```

| Code | Produced by |
|---|---|
| `max_turns_exceeded` | `*MaxTurnsError` |
| `model_behavior` | `*ModelBehaviorError` |
| `model_refusal` | `*ModelRefusalError` |
| `user_error` | `*UserError` |
| `tool_timeout` | `*ToolTimeoutError` |
| `tool_panic` | A tool panic on the fatal path (`FailureErrorFunction == nil`) |
| `guardrail_tripwire` | `*GuardrailTripwireError` |
| `sandbox_exec` | A sandbox command that failed to run |
| `mcp` | An MCP server connection or tool call |
| `unknown` | Anything else, including a plain error from your own code |

**The set is open.** Handle an unrecognized code generically — the SDK adds
codes without a breaking change, and a consumer that treats an unknown code as
impossible breaks on upgrade.

To contribute a code from your own tool, use `Classify`. It tags the error
without hiding it, so `errors.Is` and `errors.As` still reach the original:

```go
return nil, agents.Classify(agents.CodeSandboxExec, fmt.Errorf("build: %w", err))
```

An error that already carries a code is returned unchanged — the innermost
classification wins, because it knows the most about the failure.

## Error handlers

`RunOptions.ErrorHandlers` — the counterpart of Python's `Runner.run(..., error_handlers={...})` — turns selected failures into a normal completion with a fallback final output instead of an error:

- **`MaxTurns`** fires when the run exceeds its turn budget (`*MaxTurnsError`).
- **`ModelRefusal`** fires when the model refuses to respond (`*ModelRefusalError`).
- **`InvalidFinalOutput`** fires when an agent with an output type produces a final message that fails schema validation, or no final text at all (`*ModelBehaviorError`). Other model-behavior errors (e.g. an unknown tool call) are not routed here.

A handler receives the error and a `RunErrorData` snapshot (input, items so far, their input-item form, raw responses, last agent) and returns the fallback:

```go
res, err := agents.RunSync(ctx, agent, "Analyze this long transcript", agents.RunOptions{
	ModelProvider: provider,
	MaxTurns:      3,
	ErrorHandlers: agents.RunErrorHandlers{
		MaxTurns: func(ctx context.Context, in agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
			return &agents.RunErrorHandlerResult{
				FinalOutput:        "I couldn't finish within the turn limit. Please narrow the request.",
				ExcludeFromHistory: true,
			}, nil
		},
	},
})
```

The run then completes normally: output guardrails and `OnAgentEnd` hooks run on the fallback, and `res.FinalOutput` carries it. Unless `ExcludeFromHistory` is set, an assistant message with the fallback is appended to `res.NewItems` and the session (Python's `include_in_history=True` default). For an agent with an output type, `FinalOutput` must marshal to JSON that validates against the output schema — anything else fails the run with a `*UserError`.

Return `(nil, nil)` to decline recovery and keep the original error. A declined (or missing) `InvalidFinalOutput` handler keeps the empty-output default: when the model returns no final text for a structured output type, the runner runs the model again rather than failing.

```go
ErrorHandlers: agents.RunErrorHandlers{
	ModelRefusal: func(ctx context.Context, in agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
		var refusal *agents.ModelRefusalError
		errors.As(in.Error, &refusal)
		return &agents.RunErrorHandlerResult{
			FinalOutput: Recipe{Ingredients: nil, RefusalReason: refusal.Refusal},
		}, nil
	},
},
```
