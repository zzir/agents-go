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

Usage accumulates across every turn of the run, including after handoffs. Nested agent-as-tool runs track their own usage (it is **not** added to the parent's), same as Python.

## Mid-run access

The accumulating `*Usage` lives on the [run context](context.md), so tools and hooks can read progress while the run executes:

```go
budgetCheck := agents.NewFunctionTool("expensive_op", "…",
	func(ctx context.Context, tc *agents.ToolContext, args opArgs) (string, error) {
		if tc.RunContext.Usage.TotalTokens > 100_000 {
			return "", errors.New("token budget exceeded")
		}
		return run(args)
	})
```

With a shared `RunContext` (`RunOptions.RunContext`), usage even accumulates across several runs:

```go
rc := agents.NewRunContext(myAppData)
agents.Run(ctx, a1, in1, agents.RunOptions{RunContext: rc, ModelProvider: p})
agents.Run(ctx, a2, in2, agents.RunOptions{RunContext: rc, ModelProvider: p})
fmt.Println("combined tokens:", rc.Usage.TotalTokens)
```

For per-call analysis, `RunResult.RawResponses` carries each `ModelResponse` with its own `Usage`.
