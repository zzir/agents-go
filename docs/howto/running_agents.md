# Running agents

Run agents with one of three entry points:

- `agents.RunSync(ctx, agent, input, opts)` — runs the loop to completion, returns a `*RunResult`
- `agents.Run(ctx, agent, input, opts)` — the same loop as a stream you range, plus a control handle ([Streaming](streaming.md))
- `agents.ResumeRun(ctx, state, opts)` — continues a run paused for tool approval ([Human-in-the-loop](human_in_the_loop.md)); `agents.ResumeRunWith(ctx, state, opts, ctrl)` does the same on a `RunControl` you already hold

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

`RunOptions` is grouped by what each field configures, and `Conversation` in particular collects options that constrain each other ([spec §2.0b](../reference/spec.md#20b-option-grouping)). Every field is on [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#RunOptions); where each group is explained:

- **`Model`** — provider, override, run-level settings and the per-call `InputFilter`: [Models](models.md). `InputFilter` edits what one model call sends; it changes nothing a session saves and does not fire on a resumed turn.
- **`Conversation`** — the session, its read window, projectors and the server-managed alternatives: [Sessions](sessions.md) and [below](#conversations--chat-threads).
- **`Exec`** — `MaxTurns` (0 means the default, 10), `MaxToolConcurrency` (unbounded by default — cap it against downstream rate limits), `ToolNotFoundBehavior` (`ToolNotFoundReturnToModel` feeds a hallucinated tool name back as the tool output instead of failing the run), `HandoffInputFilter` (the default for every handoff without its own — [Handoffs](handoffs.md#nesting-handoff-history)), and the safety valves, turn hooks and error handlers below.
- **`Compaction`** — [Sessions](sessions.md#run-level-compaction). **`Guardrails`** — [Guardrails](guardrails.md). **`Middlewares`** — [below](#middleware). **`Observe`** — [Tracing](tracing.md). **`Log`** — [Logging](logging.md).
- **`ReasoningItemIDPolicy`** — whether reasoning-item ids survive into later turns' model input; `ReasoningItemIDOmit` is for `store=false` runs that rely on encrypted content. The choice is persisted in `RunState`.

### Local context

Two things called "context" stay separate: cancellation and deadlines ride the `context.Context` passed to `Run` (and through to every tool, guardrail and hook); your own data — the current user, a DB handle — rides `RunOptions.Context`, an `any` the SDK never inspects.

```go
type AppContext struct {
	UserID string
	DB     *sql.DB
}

res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{
	Context: &AppContext{UserID: "u_123", DB: db},
	Model:   agents.ModelOptions{Provider: provider},
})
```

Every tool, guardrail, hook and dynamic-instructions function receives the same `*agents.RunContext`; type-assert your value back:

```go
tool := agents.NewTool("whoami", "Return the current user.",
	func(ctx context.Context, tc *agents.ToolContext, _ struct{}) (string, error) {
		app := tc.RunContext.Context.(*AppContext)
		return app.UserID, nil
	})
```

`RunContext` also carries the run's live `Usage` ([Results](results.md#usage)) and its recorded `Approvals` ([Human-in-the-loop](human_in_the_loop.md)); `ToolContext` embeds it and adds the call's `ToolName`, `ToolCallID` and raw `ToolArguments`. Tools run concurrently within a turn, so a context value they mutate must be goroutine-safe.

## Conversations / chat threads

Each `Run` is one logical turn of a conversation. To carry history across runs:

1. **Use a [Session](sessions.md)** — history is loaded before the run and saved as each turn completes.
2. **Thread items manually** — build the next input from the previous result ([Results](results.md#inputs-for-the-next-turn)).
3. **Let the server keep state** — `UsePreviousResponseID: true` chains calls through the Responses API's `previous_response_id` (requires stored responses, the default); `ConversationID: "conv_..."` attaches the run to an [OpenAI conversation](sessions.md#openai-conversations-server-side). Each sends only new items per turn, and neither combines with a local `Session` — the run errors if you try.

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
*every* tool call failed; `FinalTurnWithoutTools` gives an exhausted turn budget
one more model call with no tools and no handoffs, so the model closes out in
prose instead of the run failing with `*MaxTurnsError`. What counts as a failed
turn, and why the last turn is tool-free and opt-in, is
[spec §2.7d](../reference/spec.md#27d-tool-loop-safety-valves). A tool with
`Sequential: true` makes its **whole batch** run one call at a time; see
[Tools](tools.md#adapting-a-tool-you-did-not-build).

### Truncated responses

A response cut off at the output-token limit (`status="incomplete"`, reason
`max_output_tokens`) has none of its tool calls executed or paused for approval:
each is answered with an explanation so the model resends, and the run
continues ([spec §2.7e](../reference/spec.md#27e-truncated-responses)).

## Steering a run in flight

`Run` returns a `RunControl` next to the stream. Besides `StopAfterTurn` and
`Pending`, it has three ways to put input into a run that is already going:

```go
stream, ctrl := agents.Run(ctx, agent, "research this", opts)

ctrl.Steer("actually, focus on the pricing")   // change course NOW
ctrl.NextTurn("mention the source when you cite it")  // ride along with the next turn
ctrl.FollowUp("now summarize it for a customer")      // and then do this
```

`Steer` lands on the next model call and `FollowUp` after the final output —
both extend a run that was finishing, in the **same** run; `NextTurn` rides
along with the next turn boundary if there is one, and whatever arrived too
late is reported by `ctrl.Pending()`. Injections reach the model in arrival
order, delivery is transactional across retries and resumes, and input queued
before an [approval pause](human_in_the_loop.md) rides along in
`RunState.PendingInput` ([spec §2.11b](../reference/spec.md#211b-run-control)).
A runnable program is [examples/steering](../../examples/steering/main.go).

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

`ShouldStopAfterTurn` is a predicate, not a producer: a run stopped here has
its full history saved, its final output is the turn's last message (else its
last tool output), and `RunResult.StoppedEarly` is set. The other place a run
can end early is a tool's own result, `ToolResult.Terminate`
([Tools](tools.md#returning-more-than-a-value-toolresult)). The policy belongs
to the run rather than the agent, so the same agent stops at different points
in different runs ([spec §2.3c](../reference/spec.md#23c-stopping-early)).

`PrepareNextTurn` reshapes **one** turn without mutating the `Agent`; the
`*TurnResult` it receives is read-only, and the runner owns `Snapshot.Input` —
to edit what a call sends, use `ModelOptions.InputFilter`
([spec §2.3b](../reference/spec.md#23b-turn-snapshots)).

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

**Order is behavior.** `Approval` sits *inside* `Loop` — a middleware that
resolves something about one attempt goes inside one that decides whether to
make another; the reason, the three-clause stream contract a middleware owes,
and what is deliberately NOT middleware (handoffs, guardrails, persistence,
tracing, error handlers) are [spec §2.12](../reference/spec.md#212-middleware).

**Plan mode** (`middleware.Plan`) splits a run in two: while planning, a tool
that is not read-only (`Tool.ReadOnly`, or named in `ReadOnlyTools`) stays in
the toolset but refuses when called, handoffs are hidden, and no approval is
raised. `submit_plan` is always approval-gated, and that pause IS the plan
review — `Approve` unlocks the full toolset and the same run continues,
`Reject`'s message sends the model back to planning. **Todo mode**
(`middleware.Todo`) adds `todo_write`, which replaces the whole list on every
call and reports it through `OnUpdate`. Both rewrite the entry agent only. Why
gating denies rather than hides, and what a durable-resume host persists, are
[spec §2.12](../reference/spec.md#212-middleware); a runnable program with both
is [examples/planmode](../../examples/planmode/main.go).

`middleware.Retry` re-runs the whole run from the start; `agents.NewRetryModel`
retries one model call and the run never notices ([Models](models.md)). With a
session attached, neither `Loop` nor `Retry` re-sends what the session already
holds ([spec §2.12](../reference/spec.md#212-middleware)).

For callbacks tied to a specific agent rather than the whole run, see
[`Agent.OnStart` / `Agent.OnEnd`](agents.md#per-agent-callbacks).

## Errors

All failures come back as Go errors. The SDK's typed errors —
`*MaxTurnsError`, `*ModelBehaviorError`, `*ModelRefusalError`, `*UserError`,
`*ToolTimeoutError`, `*ToolLoopError`, `*GuardrailTripwireError` — carry their
data as plain fields and are matched with `errors.As`; each is documented on
[pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#MaxTurnsError).
A run that fails after its loop started returns a `*RunError` wrapping the
cause; its `Result` is the partial progress in the same `*RunResult` shape a
finished run reports — see [Results](results.md#errors).

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

**The set is open.** Handle an unrecognized code generically — the SDK adds
codes without a breaking change, and a consumer that treats an unknown code as
impossible breaks on upgrade. The `Code*` constants are on
[pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#ErrorCode). A
context overflow the run survived is not a code but a diagnostic,
`context_overflow` ([Logging](logging.md#diagnostics-trouble-a-run-survived)).

To contribute a code from your own tool, `agents.Classify(agents.CodeSandboxExec, err)`
tags the error without hiding it from `errors.Is` / `errors.As`; an error that
already carries a code is returned unchanged
([spec §2.10](../reference/spec.md#210-errors-and-recovery)).

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

The run then completes normally: output guardrails and the agent's `OnEnd` callback run on the fallback, and `res.FinalOutput` carries it. Unless `ExcludeFromHistory` is set, an assistant message with the fallback is appended to `res.NewItems` and the session. For an agent with an output type, `FinalOutput` must marshal to JSON that validates against the output schema — anything else fails the run with a `*UserError`.

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

A runnable program is [examples/errorhandlers](../../examples/errorhandlers/main.go).
