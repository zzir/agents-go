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
