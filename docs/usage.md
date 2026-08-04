# Usage

The SDK tracks token usage for every run. Read it from the result:

```go
res, _ := agents.RunSync(ctx, agent, input, opts)
u := res.Usage
fmt.Printf("requests=%d input=%d output=%d total=%d cached=%d reasoning=%d\n",
	u.Requests, u.InputTokens, u.OutputTokens, u.TotalTokens,
	u.InputTokensDetails.CachedTokens, u.OutputTokensDetails.ReasoningTokens)
```

`Usage` fields:

| Field | Meaning |
|---|---|
| `Requests` | Number of model API calls |
| `InputTokens` / `OutputTokens` / `TotalTokens` | Token counts summed across calls |
| `InputTokensDetails.CachedTokens` | Prompt-cache hits |
| `OutputTokensDetails.ReasoningTokens` | Reasoning tokens (reasoning models) |
| `RequestUsageEntries` | Per-request usage breakdown, in order |

Usage accumulates across every turn of the run, including after handoffs, and a
nested [agent-as-tool](multi_agent.md) run's usage is folded into the parent's —
so `res.Usage` is what the whole run cost, with nothing spent off to the side.

## Where the tokens went

A total answers "what did this cost" and nothing else. Two views answer "where
did it go":

```go
for responseID, u := range res.UsageByResponse() {
	fmt.Printf("%s: %d in / %d out\n", responseID, u.InputTokens, u.OutputTokens)
}
nested := res.NestedUsage()   // spent by tools on model calls of their own
```

`NestedUsage` is already part of `res.Usage`; it says how much of the total went
somewhere other than this run's own conversation, which is the number that
explains a bill the turn count cannot.

## Usage on stored history

With a [Session](sessions.md), **exactly one entry per response carries that
response's usage** — the last one it produced. Summing `SessionEntry.Usage` over
a session therefore reproduces its true cost, and a reader estimating how large
the conversation has grown can take the most recent one as measured fact and
estimate only what follows.

`SessionEntry.NestedUsage` is kept separate from `Usage` for the same reason as
above: a nested run's tokens were spent on a different conversation, and
counting them as context would make this one look larger than anything ever
sent.

## Mid-run access

The accumulating `*Usage` lives on the [run context](context.md), so tools and hooks can read progress while the run executes:

```go
budgetCheck := agents.NewFunctionTool("expensive_op", "…",
	func(ctx context.Context, tc *agents.ToolContext, args opArgs) (string, error) {
		if tc.RunContext.Usage.Snapshot().TotalTokens > 100_000 {
			return "", errors.New("token budget exceeded")
		}
		return run(args)
	})
```

`RunContext.Usage` is the run's **live** accumulator — parallel agent-as-tool
calls fold their usage in concurrently — so mid-run readers go through
`Snapshot()`. A `RunResult.Usage` is a detached copy taken when the result was
built; reading it directly is always safe.

Across several runs, sum each result's `Usage` — every run owns a fresh
`RunContext`, so there is no shared accumulator to thread through:

```go
r1, _ := agents.RunSync(ctx, a1, in1, agents.RunOptions{Model: agents.ModelOptions{Provider: p}})
r2, _ := agents.RunSync(ctx, a2, in2, agents.RunOptions{Model: agents.ModelOptions{Provider: p}})
total := agents.NewUsage()
total.Add(r1.Usage)
total.Add(r2.Usage)
fmt.Println("combined tokens:", total.TotalTokens)
```

For raw per-call data, `RunResult.RawResponses` carries each `ModelResponse`
with its own `Usage`, `Status` and `IncompleteReason`.
