# Guardrails

A guardrail inspects a run at one or more **stages** and decides whether to let
it proceed, substitute the content, or halt.

```go
type Guardrail struct {
    Name     string
    Stages   []GuardrailStage
    Blocking bool
    Run      func(ctx context.Context, rc *RunContext, p GuardrailPayload) (GuardrailDecision, error)
}
```

There is one guardrail type for every stage. A content scanner that should see
the input, the tool arguments and the final output is **one value**, not three:

```go
scanner := agents.Guardrail{
    Name:   "pii",
    Stages: []agents.GuardrailStage{agents.StageInput, agents.StageToolInput, agents.StageOutput},
    Run: func(ctx context.Context, rc *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
        if containsPII(p) {
            return agents.Trip("pii detected"), nil
        }
        return agents.Allow(nil), nil
    },
}
```

## Stages

| Stage | When it runs | What it inspects |
|---|---|---|
| `StageInput` | First turn, before or alongside the first model call | `p.Input` |
| `StageOutput` | After the final output is produced, before it is persisted | `p.Output` |
| `StageToolInput` | After arguments are parsed, before the tool runs | `p.ToolName`, `p.Arguments` |
| `StageToolOutput` | After the tool runs, before its result reaches the model | `p.ToolName`, `p.Output` |

## Decisions

| Decision | Effect |
|---|---|
| `Allow(info)` | Proceed unchanged. |
| `Replace(msg, info)` | Substitute `msg` for the inspected content and continue. |
| `Trip(info)` | Halt the run with a `*GuardrailTripwireError`. |

What `Replace` substitutes depends on the stage:

- `StageInput` — the run input becomes a single user message carrying the text.
  For finer rewriting use a model-input filter instead.
- `StageOutput` — it becomes the run's final output.
- `StageToolInput` — the tool does **not** execute; the text becomes its result.
- `StageToolOutput` — it replaces the result sent back to the model.

`OutputInfo` rides along on every decision, including `Allow`, so callers can
read a guardrail's diagnostics whether or not it fired.

## Placement decides scope

| Where | Scope |
|---|---|
| `RunOptions.Guardrails` | The whole run, consulted first at every stage. |
| `Agent.Guardrails` | That agent's turns. Tool stages cover **every** tool the agent exposes. |
| `FunctionTool.Guardrails` | That tool only. Only its tool stages are consulted. |

Run-level and agent-level guardrails both apply; run-level ones run first.

## Typed constructors

For single-stage guardrails these are shorter and keep payload access type-safe:

```go
agents.NewInputGuardrail("len", func(ctx context.Context, input []agents.TResponseInputItem) (agents.GuardrailDecision, error) {
    if tooLong(input) {
        return agents.Trip("input too long"), nil
    }
    return agents.Allow(nil), nil
})

agents.NewOutputGuardrail("secrets", func(ctx context.Context, out any) (agents.GuardrailDecision, error) { ... })
agents.NewToolInputGuardrail("args", func(ctx context.Context, tool, argsJSON string) (agents.GuardrailDecision, error) { ... })
agents.NewToolOutputGuardrail("leak", func(ctx context.Context, tool string, out any) (agents.GuardrailDecision, error) { ... })
```

## Blocking input guardrails

By default a `StageInput` guardrail runs **concurrently** with the first model
call; a tripwire cancels that call, so the run fails without waiting for it.

Set `Blocking: true` to make it a gate that runs to completion first. Use this
when a tripwire must prevent any token spend at all:

```go
agents.Guardrail{
    Name:     "gate",
    Stages:   []agents.GuardrailStage{agents.StageInput},
    Blocking: true,
    Run:      ...,
}
```

Other stages ignore `Blocking` — they are always sequential relative to the work
they guard.

## Ordering

Guardrails at the input and output stages run **concurrently** and fail fast:
the first tripwire or error ends the wait and cancels the context handed to the
rest.

Tool-stage guardrails run **in order** and stop at the first `Replace` or
`Trip` — once one has substituted the content, running the others against the
original would be meaningless.

## Inspecting results

Every consulted guardrail produces a `GuardrailResult`, allowing decisions
included:

```go
res, err := agents.Run(ctx, agent, "hi", opts)
for _, g := range res.GuardrailResults {
    fmt.Println(g.Stage, g.Guardrail.Name, g.Decision.Action, g.Decision.OutputInfo)
}
```

Filter by `g.Stage` for one stage. Tool-stage results also carry `ToolName`,
`ToolCallID` and `Arguments`.

Results survive a human-in-the-loop pause: `RunState.GuardrailResults` carries
them across serialization, so a resumed run still reports them. The
serialization is lossy — a decoded result holds a name-only stub guardrail,
because the live `Run` func does not round-trip.

## Errors vs tripwires

A guardrail returning an **error** aborts the run with that error: the guardrail
itself failed. A guardrail returning `Trip(...)` aborts with a
`*GuardrailTripwireError`: the guardrail worked and the content was rejected.

```go
var tw *agents.GuardrailTripwireError
if errors.As(err, &tw) {
    fmt.Println(tw.Stage(), tw.Result.Guardrail.Name, tw.Result.Decision.OutputInfo)
}
```

A panicking guardrail is recovered and reported as that guardrail's error — it
never crashes the process.
