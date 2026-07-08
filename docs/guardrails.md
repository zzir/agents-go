# Guardrails

Guardrails validate what flows in and out of your agents. There are three kinds:

- **Input guardrails** check the user input before (in parallel with) the first model call
- **Output guardrails** check the agent's final output before the run returns
- **Tool guardrails** check a single tool call's arguments or result

Input/output guardrails trip a **tripwire**: the run stops immediately with a typed error, so an expensive model never wastes tokens on disallowed work.

## Input guardrails

An input guardrail receives the full model input (session history plus the new user input) and returns a `GuardrailFunctionOutput`:

{% raw %}
```go
agent.InputGuardrails = []agents.InputGuardrail{{
	Name: "math_homework_filter",
	Run: func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, input []agents.TResponseInputItem) (agents.GuardrailFunctionOutput, error) {
		verdict, err := classify(ctx, input) // e.g. a cheap model call
		if err != nil {
			return agents.GuardrailFunctionOutput{}, err
		}
		return agents.GuardrailFunctionOutput{
			OutputInfo:        verdict,           // anything you want to inspect later
			TripwireTriggered: verdict.IsHomework, // true halts the run
		}, nil
	},
}}
```
{% endraw %}

All input guardrails run **concurrently with the first model call** (matching the Python SDK); a tripwire fails the run with `*agents.InputGuardrailTripwireError`, which carries the result:

```go
_, err := agents.Run(ctx, agent, input, opts)
var trip *agents.InputGuardrailTripwireError
if errors.As(err, &trip) {
	log.Printf("blocked by %s: %+v", trip.Result.Guardrail.Name, trip.Result.Output.OutputInfo)
}
```

Input guardrails are the *first* agent's: they only run when the agent is the start of the run, so different agents in a handoff chain can carry their own.

Set `Blocking: true` on a guardrail to run it **to completion before** the first model call — a gate, so a tripwire prevents any token spend. The zero value (`false`) keeps the default concurrent behavior (`Blocking` is the inverse of Python's `run_in_parallel`, whose default `True` can't be a Go bool zero value). When a concurrent guardrail trips, the in-flight model call is cancelled and is neither billed nor surfaced to `OnLLMEnd`.

## Run-level guardrails

`RunOptions.InputGuardrails` and `RunOptions.OutputGuardrails` apply to every run regardless of which agent handles it, running alongside the agent's own (the run-level ones first) — the counterpart of Python's `RunConfig.input_guardrails` / `output_guardrails`:

```go
agents.Run(ctx, agent, input, agents.RunOptions{
	ModelProvider:    provider,
	InputGuardrails:  []agents.InputGuardrail{moderation},
	OutputGuardrails: []agents.OutputGuardrail{piiCheck},
})
```

## Inspecting guardrail results

A guardrail left with an empty `Name` is reported under a fixed label — `"input_guardrail"` or `"output_guardrail"` — in its result and in the tripwire error, so `trip.Result.Guardrail.Name` is never blank. (Go has no function-name reflection, so unlike Python it cannot fall back to the guardrail function's name; give each guardrail a `Name` to tell them apart.)

Every guardrail's result — run-level and agent-level, tripping or not — is exposed on the `RunResult` so you can read a non-tripping guardrail's `OutputInfo` (e.g. moderation scores):

```go
res, _ := agents.Run(ctx, agent, input, opts)
for _, r := range res.InputGuardrailResults {
	log.Printf("%s: %+v", r.Guardrail.Name, r.Output.OutputInfo)
}
for _, r := range res.OutputGuardrailResults {
	log.Printf("%s checked %v: %+v", r.Guardrail.Name, r.AgentOutput, r.Output.OutputInfo)
}
```

Tool guardrail results are exposed the same way: `res.ToolInputGuardrailResults` and `res.ToolOutputGuardrailResults` hold every tool guardrail's result across the run — allow, reject and raise alike — each carrying the `ToolName`/`ToolCallID` it ran for and its `Output` (see [Tool guardrails](#tool-guardrails)). All four result slices are carried on `RunState` and rehydrated on resume, so a run paused for approval still reports its earlier guardrail results after `ResumeRun`.

## Output guardrails

Output guardrails receive the final output value and run before the result is returned (and before it is saved to a session):

{% raw %}
```go
agent.OutputGuardrails = []agents.OutputGuardrail{{
	Name: "no_pii",
	Run: func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, output any) (agents.GuardrailFunctionOutput, error) {
		return agents.GuardrailFunctionOutput{TripwireTriggered: containsPII(output)}, nil
	},
}}
```
{% endraw %}

A tripwire fails the run with `*agents.OutputGuardrailTripwireError`. Output guardrails are the *last* agent's — the one that produced the final output.

## Tool guardrails

Tool guardrails scope validation to a single tool. Unlike run-level guardrails they can **reject content** without killing the run: the tool is skipped (or its output replaced) and a message goes back to the model instead.

{% raw %}
```go
t := agents.NewFunctionTool("send_email", "…", sendEmail)
t.InputGuardrails = []agents.ToolInputGuardrail{{
	Name: "block_external_recipients",
	Run: func(ctx context.Context, rc *agents.RunContext, data agents.ToolInputGuardrailData) (agents.ToolGuardrailFunctionOutput, error) {
		if isExternal(data.Arguments) {
			return agents.RejectToolContent("External recipients are not allowed.", nil), nil
		}
		return agents.AllowTool(nil), nil
	},
}}
t.OutputGuardrails = []agents.ToolOutputGuardrail{{
	Name: "redact_secrets",
	Run: func(ctx context.Context, rc *agents.RunContext, data agents.ToolOutputGuardrailData) (agents.ToolGuardrailFunctionOutput, error) {
		if leaks(data.Output) {
			return agents.RejectToolContent("[redacted]", nil), nil
		}
		return agents.AllowTool(nil), nil
	},
}}
```
{% endraw %}

Three behaviors, built with helpers:

| Helper | Effect |
|---|---|
| `agents.AllowTool(info)` | Proceed normally (zero value behavior) |
| `agents.RejectToolContent(msg, info)` | Skip the tool / replace its output; `msg` goes to the model |
| `agents.RaiseToolException(info)` | Halt the run with `*agents.ToolGuardrailTripwireError` |

For approval-gated tools, input guardrails normally run only **after** the human approves. Set `RunOptions.PreApprovalToolInputGuardrails` to also run them **before** the approval interruption is surfaced, so a guardrail rejection resolves the call without a human round-trip — see [Human-in-the-loop](human_in_the_loop.md#pre-approval-guardrails).

## Errors vs tripwires

Returning a non-nil `error` from any guardrail aborts the run with that error (it means the guardrail itself failed). A tripwire is a deliberate verdict and produces the typed tripwire error instead.
