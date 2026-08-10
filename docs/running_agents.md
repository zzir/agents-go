# Running agents

Run agents with one of three entry points:

- `agents.RunSync(ctx, agent, input, opts)` — runs the loop to completion, returns a `*RunResult`
- `agents.Run(ctx, agent, input, opts)` — the same loop as a stream you range, plus a control handle ([Streaming](streaming.md))
- `agents.ResumeRun(ctx, state, opts)` — continues a run paused for tool approval ([Human-in-the-loop](human_in_the_loop.md))

`input` is either a `string` (treated as a user message) or a `[]agents.InputItem` — the OpenAI Responses API item list.

```go
res, err := agents.RunSync(ctx, agent, "Write a haiku about recursion.", agents.RunOptions{
	Model: agents.ModelOptions{Provider: provider},
})
```

## The agent loop

`Run` executes this loop:

1. Call the model for the current agent with the conversation so far.
2. If the model produced a final output (a message with no pending tool calls, matching the agent's output type), the loop ends.
3. If the model requested a handoff, switch the current agent and loop.
4. Otherwise execute the tool calls (concurrently), append their results, and loop.

If the number of turns exceeds the budget, the run fails with `*agents.MaxTurnsError` — unless a [`MaxTurns` error handler](#error-handlers) recovers it with a fallback final output.

## Run options

`RunOptions` is grouped by what each field configures — the groups are not cosmetic; `Conversation` in particular collects options that constrain each other:

```go
type RunOptions struct {
	Model        ModelOptions        // Provider / Override / Settings / InputFilter
	Conversation ConversationOptions // Session, server-managed state, projectors
	Exec         ExecOptions         // MaxTurns, tool policies, error handlers, injection points
	Compaction   CompactionOptions   // shrink context as the conversation grows (docs: Sessions)
	Guardrails   []Guardrail         // run-level guardrails, before each agent's own
	Middlewares  []RunMiddleware     // wrap the run, outermost first
	Observe      ObserveOptions      // opt-in tracing (docs: Tracing)
	Log          LogConfig           // the SDK's own structured logging (docs: Logging)
	Context      any                 // your app data, threaded through tools/guardrails/hooks
}
```

The commonly reached-for knobs, by group:

- **`Model.Provider`** resolves agent model names (required unless every agent sets `ModelImpl`, or `Model.Override` is set); **`Model.Settings`** is a run-level `*ModelSettings` merged over each agent's own; **`Model.InputFilter`** (a `CallModelInputFilter`) runs just before each model call to edit the system instructions and input items actually sent (e.g. trim tokens, inject context). It does not change what a [session](sessions.md) saves, and does not fire on a HITL-resumed turn.
- **`Conversation.Session`** supplies and persists history; **`Conversation.Settings`** (a `session.Settings` value, e.g. `Limit`) caps how much of it a run loads, and its zero value reads the whole history; **`Conversation.UsePreviousResponseID`** / **`Conversation.ConversationID`** are the server-managed alternatives ([below](#conversations--chat-threads)); **`Conversation.Projectors`** overrides what entry kinds the model reads ([Sessions](sessions.md#projection-what-the-model-reads)).
- **`Exec.MaxTurns`** is the turn budget (0 means `DefaultMaxTurns`, 10); exceeding it fails the run unless an error handler recovers it.
- **`Exec.MaxToolConcurrency`** bounds how many of a turn's function tools run at once (they otherwise all run in parallel) — useful against downstream rate limits.
- **`Exec.ToolNotFoundBehavior`** defaults to `ToolNotFoundError` (a hallucinated tool name aborts the run). Set `ToolNotFoundReturnToModel` to instead feed an error back as the tool output so the model can correct itself.
- **`Exec.HandoffInputFilter`** applies to any handoff that doesn't set its own `Handoff.InputFilter` — e.g. `agents.NestHandoffHistory(...)` to fold prior history across every handoff ([Handoffs](handoffs.md#nesting-handoff-history)).
- **`Exec.ErrorHandlers`** recovers max-turns/refusal/invalid-output failures with a fallback final output ([below](#error-handlers)); **`Exec.PrepareNextTurn`** / **`Exec.ShouldStopAfterTurn`** reshape or stop the run at turn boundaries; **`Exec.Overflow`** turns a context-overflow failure into compact-and-retry ([Sessions](sessions.md)).
- **`ReasoningItemIDPolicy`** controls whether reasoning-item ids survive when run items are converted back into model input on later turns. `ReasoningItemIDPreserve` (the default) keeps them; `ReasoningItemIDOmit` strips them — useful for `store=false` runs whose server-side ids are no longer valid and that rely on encrypted content. The choice is persisted across HITL interruptions in `RunState`.

## Conversations / chat threads

Each `Run` is one logical turn of a conversation. To carry history across runs you can:

1. **Use a [Session](sessions.md)** — history is loaded before the run and saved incrementally as it proceeds (each turn as it completes):

   ```go
   sess := session.NewInMemorySession()
   agents.Run(ctx, agent, "What city is the Golden Gate Bridge in?", agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: p}})
   agents.Run(ctx, agent, "What state is it in?", agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: p}})
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

The `context.Context` you pass governs the whole run: cancel it to abort between turns, mid-stream, and inside tool calls (tools receive the same context).

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
defer cancel()
res, err := agents.RunSync(ctx, agent, input, opts)
```

Per-tool timeouts are a [tool-level setting](tools.md#timeouts).

## Tool-loop safety valves

`ExecOptions.ToolLoop` bounds the ways a run can keep going without getting
anywhere:

```go
opts.Exec.ToolLoop = agents.ToolLoopPolicy{
	MaxConsecutiveErrorTurns: 3,    // default; -1 disables
	FinalTurnWithoutTools:    true, // off by default
}
```

`MaxConsecutiveErrorTurns` aborts with a `*ToolLoopError` after N turns in which
*every* tool call failed. Any success clears the counter, and a turn with no
tool calls is neither counted nor cleared. Without it, a model calling a broken
tool spends the whole turn budget rediscovering that it is broken.

`FinalTurnWithoutTools` gives an exhausted turn budget one more model call **with
no tools and no handoffs**, so the model closes out in prose rather than the run
failing with `*MaxTurnsError`. Tool-free is the point — offered a tool it would
call one. It is opt-in because a turn budget is sometimes a cost ceiling, and
this spends a call it said not to spend.

A tool with `Sequential: true` makes its **whole batch** run one call
at a time; see [Tools](tools.md#adapting-a-tool-you-did-not-build).

### Truncated responses

A model response cut off at the output-token limit
(`status="incomplete"`, reason `max_output_tokens`) has **none of its tool calls
executed**. Each is answered with an explanation so the model resends.

This is correctness, not policy: a truncated response looks ordinary — items
present, no error — but its tail may be half-formed, and a tool call's arguments
are exactly the kind of tail that gets cut. Truncation is fed back to the model
rather than failing the run; every other incomplete reason still fails.

## Steering a run in flight

`Run` returns a `RunControl` next to the stream. Besides `StopAfterTurn` and the
progress accessors, it has three ways to put input into a run that is already
going:

```go
stream, ctrl := agents.Run(ctx, agent, "research this", opts)

ctrl.Steer("actually, focus on the pricing")   // change course NOW
ctrl.NextTurn("mention the source when you cite it")  // ride along with the next turn
ctrl.FollowUp("now summarize it for a customer")      // and then do this
```

| | When it lands | Extends a run that was finishing |
|---|---|---|
| `Steer` | the next model call, whatever the run is doing | **yes** |
| `NextTurn` | the next turn boundary, if there is one | no |
| `FollowUp` | after the final output, in the **same** run | **yes** |

`FollowUp` continues the same run rather than starting a new one, so the trace,
the usage total and the session stay one thing.

Injections reach the model **in the order they were made**, across all three
methods, and delivery is transactional: input consumed by an attempt that then
fails (a middleware retry, a failed resume) is returned to the queue and
delivered by the next attempt — nothing lost, nothing doubled.

Injected input is recorded as the user's, so a reopened session shows what was
actually said rather than an answer to a question nobody asked. Whatever a run
did not consume — a `NextTurn` that arrived as the run was ending — is reported
by `ctrl.Pending()` instead of vanishing.

Input queued before a run pauses for [approval](human_in_the_loop.md) rides
along in `RunState.PendingInput` — across `RunState` serialization too — and
is delivered on resume.

## Turn hooks

A turn is resolved into a `TurnSnapshot` — agent, model, settings, instructions,
prompt, tools, handoffs, output schema, input — before the model is called. Two
`ExecOptions` hooks see it at the **save point**: the moment the turn's message
and every tool result are persisted, and the next model call has not happened.

```go
opts.Exec.ShouldStopAfterTurn = func(ctx context.Context, tr *agents.TurnResult) (bool, error) {
	return slices.Contains(tr.ToolCallNames(), "save_report"), nil
}

opts.Exec.PrepareNextTurn = func(ctx context.Context, tr *agents.TurnResult) (*agents.TurnSnapshot, error) {
	next := *tr.Snapshot
	next.Tools = nil                    // withdraw the tools now that they have run
	next.Model = cheapModel             // finish on something cheaper
	return &next, nil
}
```

`PrepareNextTurn` applies to **one** turn; the turn after resolves afresh. It
changes the run without mutating the `Agent`, which a concurrent run may be
reading.

The `*TurnResult` a hook receives is a **read-only view of the finished turn**.
Writing to its fields changes nothing — not the run's final output, not what
the other hook sees. To shape the next turn, return a `TurnSnapshot`.

**The runner owns `Snapshot.Input`** and replaces whatever a returned snapshot
carries. A prepared snapshot is nearly always a copy of the previous turn's, so
honoring its input would replay that turn with the tool call and its output
missing. To edit what a call sends, use `ModelOptions.InputFilter`, which runs
per turn on the input the loop built.

## Middleware

`RunOptions.Middlewares` wraps a run, outermost first. It is where optional
policy lives — logging, retrying, recovering, rewriting input — so the loop does
not grow a field and a branch for each one.

```go
mw := agents.RunMiddlewareFunc(func(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
	in.Opts.Exec.MaxTurns = 5          // adjust the run
	return next(ctx, in)               // ...or do not call next at all
})

agents.RunSync(ctx, agent, "hi", agents.RunOptions{
	Middlewares: []agents.RunMiddleware{middleware.Retry{MaxAttempts: 2}, mw},
})
```

A middleware that only observes calls `next` and re-yields the stream unchanged;
one that intervenes can inspect events, replace them, run the loop twice, or
refuse before the model is ever called.

### Built-in middleware

`agents/middleware` ships the run-level policies that come up most:

| | What it does |
|---|---|
| `middleware.Loop` | Re-runs the agent until an `Evaluator` accepts the answer, feeding each rejected attempt back with the reason |
| `middleware.Approval` | Answers approval interruptions from a standing `ApprovalPolicy` and resumes, so the caller only sees the pauses the policy declined |
| `middleware.Retry` | Re-runs a **failed** run |
| `middleware.Plan` | Plan mode: read-only exploration, a plan submitted through `submit_plan` pauses for approval, and approval unlocks the toolset in the same run |
| `middleware.Todo` | Has the agent keep a working todo list through `todo_write`; the host observes it via `OnUpdate` |

```go
import "github.com/zzir/agents-go/agents/middleware"

opts.Middlewares = []agents.RunMiddleware{
	middleware.Retry{MaxAttempts: 3},
	middleware.Approval{Policy: middleware.AllowTools("read_file", "list_files")},
	middleware.Loop{Evaluate: func(ctx context.Context, res *agents.RunResult) (middleware.Evaluation, error) {
		if looksRight(res.FinalOutputString()) {
			return middleware.Stop(), nil
		}
		return middleware.Continue("the answer must cite a source"), nil
	}},
}
```

**Order is behavior, not style.** `Approval` must sit *inside* `Loop`: outside
it, the loop's first attempt comes back paused with no answer, and the
evaluator judges an empty string. The rule of thumb is that a middleware which
resolves something about one attempt goes inside one that decides whether to
make another attempt.

`Loop` is the shape middleware exists for: the run loop knows when a model has
*finished talking* and nothing more, while "good enough" is the caller's
question — a critic agent, a schema check, a compiler.

**Plan mode** (`middleware.Plan`) splits a run into two phases with one pause
between them. While planning, only the tools named in `ReadOnlyTools`
(`DefaultReadOnlyTools` when nil) are visible — direct tools through their
enabled hook, MCP tools by filtering each turn's listing, handoffs through
`Handoff.IsEnabled` (a target's full toolset would be a side door out of plan
mode) — plus a `submit_plan` tool that is always approval-gated. The pause IS the plan
review: an interruption whose tool is `middleware.PlanToolName` carries the
plan in its arguments; `Approve` unlocks the full toolset and the same run
continues into execution, `Reject`'s message sends the model back to planning
with the write tools still hidden. Read-only-ness is a name list, not a tool
capability — tools carry no side-effect marker, and a list the caller can see
and edit beats an interface nobody remembers to implement.

**Todo mode** (`middleware.Todo`) adds a `todo_write` tool and a preamble
telling the model to keep a working list. Every call replaces the whole list —
the model always sends every item, which is simpler to prompt for and
impossible to desynchronize. The host renders it from `OnUpdate` (or reads the
calls off the stream); a malformed list is refused whole, so an observer never
sees a half-applied update. Both middlewares rewrite the entry agent only;
handoff targets keep their own toolset. See
[examples/planmode](../examples/planmode/main.go) for both together.

`middleware.Retry` and `agents.NewRetryModel` are different and usually both
right. The model decorator retries one call (a 429, a dropped connection) and
the run never notices; the middleware retries the whole run, which is what a
failure the loop could not absorb needs. A failed run is retried from the
start, not resumed — resuming means guessing which side effects already
happened, and the SDK cannot know.

**What is deliberately not middleware**: handoffs, guardrails, session
persistence, tracing, and `ExecOptions.ErrorHandlers`. Those are not policy
layered over the loop, they *are* the loop — a handoff changes which agent the
state machine is in, guardrails race the model call and cancel it, persistence
has a boundary only the loop knows, and an error handler needs the run's
in-flight items to build `RunErrorData` and the loop's completion path to
persist what it recovers. A middleware sees a terminal error and can
reconstruct neither. Expressing them as middleware would turn invariants into
implicit protocols between wrappers.

For callbacks tied to a specific agent rather than the whole run, see
[`Agent.OnStart` / `Agent.OnEnd`](agents.md#per-agent-callbacks).

## Errors

All failures come back as Go errors. The SDK's typed errors carry their data as plain fields and are matched with `errors.As`:

| Error | Meaning |
|---|---|
| `*MaxTurnsError` | Turn budget exhausted |
| `*ModelBehaviorError` | The model did something invalid (unknown tool, malformed structured output, truncated stream) |
| `*ModelRefusalError` | The model refused to respond; carries the refusal text |
| `*UserError` | You used the SDK incorrectly (e.g. no model provider, invalid output schema) |
| `*ToolTimeoutError` | A tool exceeded its `Tool.Timeout` |
| `*GuardrailTripwireError` | A guardrail tripped; `Stage()` says where |

A run that fails after its loop started returns a `*RunError` wrapping the cause; its `Result` field is the partial progress (input, items generated so far, raw responses, last agent, usage) in the same `*RunResult` shape a finished run reports — see [Results](results.md#errors).

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
| `tool_panic` | A tool panic, whether it aborted the run or was recovered |
| `tool_loop` | `*ToolLoopError` — every tool failed on N consecutive turns |
| `guardrail_tripwire` | `*GuardrailTripwireError` |
| `sandbox_exec` | A sandbox command that failed to run |
| `mcp` | An MCP server connection or tool call |
| `context_overflow` | Reported as a diagnostic when a run compacted and retried after the context did not fit |
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

`RunOptions.Exec.ErrorHandlers` turns selected failures into a normal completion with a fallback final output instead of an error:

- **`MaxTurns`** fires when the run exceeds its turn budget (`*MaxTurnsError`).
- **`ModelRefusal`** fires when the model refuses to respond (`*ModelRefusalError`).
- **`InvalidFinalOutput`** fires when an agent with an output type produces a final message that fails schema validation, or no final text at all (`*ModelBehaviorError`). Other model-behavior errors (e.g. an unknown tool call) are not routed here.

A handler receives the error and a `RunErrorData` snapshot (input, items so far, their input-item form, raw responses, last agent) and returns the fallback:

```go
res, err := agents.RunSync(ctx, agent, "Analyze this long transcript", agents.RunOptions{
	Model: agents.ModelOptions{Provider: provider},
	Exec: agents.ExecOptions{
		MaxTurns: 3,
		ErrorHandlers: agents.RunErrorHandlers{
			MaxTurns: func(ctx context.Context, in agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
				return &agents.RunErrorHandlerResult{
					FinalOutput:        "I couldn't finish within the turn limit. Please narrow the request.",
					ExcludeFromHistory: true,
				}, nil
			},
		},
	},
})
```

The run then completes normally: output guardrails and `OnAgentEnd` hooks run on the fallback, and `res.FinalOutput` carries it. Unless `ExcludeFromHistory` is set, an assistant message with the fallback is appended to `res.NewItems` and the session. For an agent with an output type, `FinalOutput` must marshal to JSON that validates against the output schema — anything else fails the run with a `*UserError`.

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
