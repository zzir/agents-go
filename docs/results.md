# Results

`agents.Run` (and `FinalResult()` of a streamed run) returns a `*agents.RunResult`:

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

Because handoffs can route to agents with different output types, `FinalOutput` is typed `any` — the type assertion documents which agent you expect to have answered, just like the Python SDK's `final_output_as`.

## New items

`NewItems` is everything the run generated, in order. Each `RunItem` wraps a raw Responses API item and knows which agent produced it. Type-switch on the concrete types:

```go
for _, item := range res.NewItems {
	switch it := item.(type) {
	case *agents.MessageOutputItem:
		fmt.Println("assistant:", it.Text())
	case *agents.ToolCallItem:
		fmt.Println("tool call:", it.FunctionCall().Name)
	case *agents.ToolCallOutputItem:
		fmt.Println("tool output:", it.Output)
	case *agents.HandoffCallItem, *agents.HandoffOutputItem:
		// the model requested / completed a handoff
	case *agents.ReasoningItem:
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
res2, err := agents.Run(ctx, res.LastAgent, next, opts)
```

(With a [Session](sessions.md) this bookkeeping is automatic.)

## Last agent

`LastAgent` is the agent that finished the run — useful after handoffs, e.g. to start the next user turn with the specialist that handled the last one.

## Usage

`res.Usage` aggregates token usage across every model call in the run; see [Usage](usage.md).

## Errors

When a run fails, the returned error usually embeds `agents.AgentsError` with a `Details` field carrying the partial run state:

```go
res, err := agents.Run(ctx, agent, input, opts)
if err != nil {
	if ae, ok := agents.AsAgentsError(err); ok && ae.Details != nil {
		log.Printf("failed after %d items, usage so far: %+v", len(ae.Details.NewItems), ae.Details.Usage)
	}
}
```

> Note: match concrete error types with `errors.As` (`*agents.MaxTurnsError`, `*agents.ModelBehaviorError`, …). Wrapped errors (e.g. from failed tools) are unwrapped automatically. The full list is in [Running agents](running_agents.md#errors).

## Interruptions and state

When a tool requires human approval, the run returns with `Interruptions` non-empty and `State` set instead of a final output. See [Human-in-the-loop](human_in_the_loop.md).
