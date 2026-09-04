# Results

`agents.RunSync` returns a `*agents.RunResult`; a stream delivers the same value as its terminal `*RunCompletedEvent`. The fields are on [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#RunResult); what they mean beyond their types:

- `Input` is the first model call's input — history plus the new input, possibly rewritten by a handoff filter — and `NewItems` is everything the run generated after it, in order.
- `FinalOutput` is typed `any`: a string for plain-text agents, the decoded value for `OutputType` agents ([below](#final-output)).
- `Usage` is a detached copy, never the live accumulator ([below](#usage)).
- `GuardrailResults` holds every result across all stages, allowing decisions included ([Guardrails](guardrails.md)).
- `Interruptions` and `State` are set only on a run paused for approval ([below](#interruptions-and-state)); `Diagnostics` is trouble the run survived ([Logging](logging.md#diagnostics-trouble-a-run-survived)); `StoppedEarly` says the caller stopped it at a turn boundary ([Running agents](running_agents.md#turn-hooks)); `AgentToolInvocation` names the parent tool call when the result came from an agent-as-tool run.

## Final output

For plain-text agents:

```go
fmt.Println(res.FinalOutputString())
```

For agents with an `OutputType`, the value is already decoded and validated:

```go
event, ok := agents.FinalOutputAs[CalendarEvent](res)
if !ok {
	// FinalOutput was not a CalendarEvent (e.g. a different agent answered after a handoff)
}
```

Because handoffs can route to agents with different output types, `FinalOutput` is typed `any` — the type assertion documents which agent you expect to have answered; `FinalOutputAs[T]` is the checked form.

## New items

`NewItems` is everything the run generated, in order. Each `RunItem` wraps a raw Responses API item and knows which agent produced it. Switch on `Kind`:

```go
for _, it := range res.NewItems {
	switch it.Kind {
	case agents.ItemMessage:
		fmt.Println("assistant:", it.Text())
	case agents.ItemToolCall:
		fmt.Println("tool call:", it.FunctionCall().Name)
	case agents.ItemToolCallOutput:
		fmt.Println("tool output:", it.Output)
	case agents.ItemHandoffCall, agents.ItemHandoffOutput:
		// the model requested / completed a handoff
	case agents.ItemReasoning:
		// reasoning trace from a reasoning model
	}
}
```

## Inputs for the next turn

To continue the conversation manually, convert items back to input form and append the next user message:

```go
next := res.Input
for _, it := range res.NewItems {
	in, err := it.ToInputItem()
	if err != nil { /* handle */ }
	next = append(next, in)
}
next = append(next, agents.InputItemsFromText("And why is that?")...)
res2, err := agents.RunSync(ctx, res.LastAgent, next, opts)
```

(With a [Session](sessions.md) this bookkeeping is automatic.)

## Last agent

`LastAgent` is the agent that finished the run — useful after handoffs, e.g. to start the next user turn with the specialist that handled the last one.

## Usage

`res.Usage` is what the whole run cost: every model call, across handoffs, with a nested [agent-as-tool](multi_agent.md) run's usage folded in ([spec §2.8](../reference/spec.md#28-nested-agent-as-tool-attribution)).

```go
u := res.Usage
fmt.Printf("requests=%d input=%d output=%d total=%d cached=%d reasoning=%d\n",
	u.Requests, u.InputTokens, u.OutputTokens, u.TotalTokens,
	u.InputTokensDetails.CachedTokens, u.OutputTokensDetails.ReasoningTokens)
```

Two views say where the tokens went rather than what they cost:

```go
for responseID, u := range res.UsageByResponse() {
	fmt.Printf("%s: %d in / %d out\n", responseID, u.InputTokens, u.OutputTokens)
}
nested := res.NestedUsage() // spent by tools on model calls of their own; already inside res.Usage
```

With a [Session](sessions.md), exactly one entry per response carries that response's `Usage` — the last one — so summing `session.Entry.Usage` reproduces the cost, and `Entry.NestedUsage` stays separate ([spec §2.7f](../reference/spec.md#27f-usage-attribution)).

**Mid-run**, the live accumulator is `RunContext.Usage` — parallel agent-as-tool calls fold into it concurrently, so read it through `Snapshot()`:

```go
budgetCheck := agents.NewTool("expensive_op", "…",
	func(ctx context.Context, tc *agents.ToolContext, args opArgs) (string, error) {
		if tc.RunContext.Usage.Snapshot().TotalTokens > 100_000 {
			return "", errors.New("token budget exceeded")
		}
		return run(args)
	})
```

Across runs, sum each result's `Usage` into an `agents.NewUsage()` with `Add` — every run owns a fresh accumulator. For raw per-call data, `RawResponses` carries each `ModelResponse` with its own `Usage`, `Status` and `IncompleteReason`.

## Errors

A run that fails after its loop started returns a `*agents.RunError` wrapping the cause. Its `Result` is the partial progress in the same `*RunResult` shape a finished run reports — `FinalOutput` is nil, everything else is what the run produced before failing:

```go
res, err := agents.RunSync(ctx, agent, input, opts)
if err != nil {
	if re, ok := errors.AsType[*agents.RunError](err); ok {
		log.Printf("failed after %d items, usage so far: %+v", len(re.Result.NewItems), re.Result.Usage)
	}
}
```

> Note: match concrete error types with `errors.As` (`*agents.MaxTurnsError`, `*agents.ModelBehaviorError`, …). Wrapped errors (e.g. from failed tools) are unwrapped automatically. The full list is in [Running agents](running_agents.md#errors).

## Interruptions and state

When a tool requires human approval, the run returns with `Interruptions` non-empty and `State` set instead of a final output. See [Human-in-the-loop](human_in_the_loop.md).
