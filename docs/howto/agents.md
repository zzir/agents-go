# Agents

Agents are the core building block. An agent is an LLM configured with instructions, tools, guardrails and handoffs. An agent is a plain struct literal — only `Name` is required, and zero values are sensible defaults.

## Basic configuration

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up"`
}

weather := agents.NewTool("get_weather", "Look up the weather.",
	func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
		return "Sunny in " + args.City, nil
	})

agent := &agents.Agent{
	Name:         "Haiku agent",
	Instructions: agents.StaticInstructions("Always respond in haiku form."),
	Model:        "gpt-4o-mini",
	Tools:        []*agents.Tool{weather},
}
```

Every field is on [pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#Agent). The ones with a page of their own: `Model` / `ModelImpl` and `ModelSettings` ([Models](models.md)), `Tools` ([Tools](tools.md)), `MCPServers` ([MCP](mcp.md)), `Handoffs` ([Handoffs](handoffs.md)), `Guardrails` — one list across all stages ([Guardrails](guardrails.md)); `OutputType` and `OnStart` / `OnEnd` are below.

## Dynamic instructions

`Instructions` is a func type, so the system prompt can be computed per run from the [run context](running_agents.md#local-context) — assign a function directly:

```go
agent.Instructions = func(ctx context.Context, rc *agents.RunContext, a *agents.Agent) (string, error) {
	user := rc.Context.(*MyAppContext)
	return "The user's name is " + user.Name + ". Help them with their questions.", nil
}
```

`agents.StaticInstructions("...")` wraps the fixed-string case.

## Structured output types

By default agents produce plain text (`string`). Set `OutputType` to request a typed result, validated against a reflected JSON schema in strict mode:

```go
type CalendarEvent struct {
	Name         string   `json:"name"`
	Date         string   `json:"date"`
	Participants []string `json:"participants"`
}

agent := &agents.Agent{
	Name:         "Calendar extractor",
	Instructions: agents.StaticInstructions("Extract calendar events from text."),
	OutputType:   agents.OutputType[CalendarEvent](),
}

res, _ := agents.RunSync(ctx, agent, input, opts)
event, ok := agents.FinalOutputAs[CalendarEvent](res)
```

Notes:

- Non-object roots (slices, primitives, pointers) are transparently wrapped in a `{"response": ...}` envelope, because the API requires an object root; `ValidateJSON` unwraps it.
- `agents.OutputTypeNonStrict[T]()` disables strict-mode schema rewriting for types strict mode cannot express (e.g. maps with arbitrary keys).
- Schema generation failures (recursive types, `map` roots in strict mode) panic in `OutputType`, as they do in `NewTool`: the type is a bug a test surfaces. `NewDynamicOutputSchema` takes a schema as data and returns the error instead.

## Stopping after tools run

By default a turn's tool results go back to the model for another turn. A
**tool** can end the run on its own result (`ToolResult.Terminate`, honored only
when every tool in the batch agrees — [Tools](tools.md#returning-more-than-a-value-toolresult)),
and a **run** can end at any turn boundary through
`ExecOptions.ShouldStopAfterTurn` ([Running agents](running_agents.md#turn-hooks));
there is deliberately no agent-level setting ([spec §2.3c](../reference/spec.md#23c-stopping-early)).

To prevent infinite tool loops, once an agent has called a tool the runner leaves `tool_choice` unset on its later turns (so a `"required"` or specific-tool setting cannot loop forever). Set `Agent.DisableToolChoiceReset = true` to keep `tool_choice` as configured on every turn.

## Per-agent callbacks

Two optional fields fire around this agent's participation in a run:

```go
agent.OnStart = func(ctx context.Context, rc *agents.RunContext) error {
	return checkQuota(rc)   // returning an error aborts the run
}
agent.OnEnd = func(ctx context.Context, rc *agents.RunContext, output any) error {
	return audit(output)
}
```

They are per-**agent**, which is why they exist as fields rather than being
folded into middleware: a handoff swaps the agent, and with it these callbacks,
in a way run-level middleware cannot express. `OnEnd` fires on the agent that
produced the final output — after a handoff, that is the agent handed *to*.

There are no other lifecycle hooks; each moment has a home that can do more
than observe:

| To see | Use |
|---|---|
| an agent taking over (a handoff) | `*AgentUpdatedStreamEvent` on the stream |
| a tool about to run / its result | `Guardrail{Stages: StageToolInput/StageToolOutput}` — can **rewrite**, not only refuse |
| what a model call sends / receives | `RunOptions.Model.InputFilter` (can rewrite), `*RawResponsesStreamEvent` |
| the run finishing | `*RunCompletedEvent` |

## Cloning

`Clone` returns a shallow copy — replace (rather than append to) slices when customizing:

```go
pirate := agent.Clone()
pirate.Name = "Pirate"
pirate.Instructions = agents.StaticInstructions("Talk like a pirate.")
```
