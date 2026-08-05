# Results

`agents.RunSync` returns a `*agents.RunResult`; a stream delivers the same value as its terminal `*RunCompletedEvent`:

```go
type RunResult struct {
	Input         []TResponseInputItem // first model call's input (history + new input; may be rewritten by handoff filters)
	NewItems      []RunItem            // everything generated during the run
	RawResponses  []*ModelResponse     // raw model responses, in order
	FinalOutput   any                  // string for plain-text agents, decoded value for OutputType agents
	LastAgent     *Agent               // the agent that produced the final output
	Usage         *Usage               // aggregated token usage
	Interruptions []*ToolApprovalItem  // pending tool approvals (HITL), empty for completed runs
	State         *RunState            // resumable state when paused for approvals, else nil
}
```

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

`res.Usage` aggregates token usage across every model call in the run; see [Usage](usage.md).

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
