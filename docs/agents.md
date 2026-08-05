# Agents

Agents are the core building block. An agent is an LLM configured with instructions, tools, guardrails and handoffs. Unlike the Python SDK's dataclass, a Go agent is a plain struct literal — only `Name` is required, and zero values are sensible defaults.

## Basic configuration

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up"`
}

weather := agents.NewFunctionTool("get_weather", "Look up the weather.",
	func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
		return "Sunny in " + args.City, nil
	})

agent := &agents.Agent{
	Name:         "Haiku agent",
	Instructions: agents.StaticInstructions("Always respond in haiku form."),
	Model:        "gpt-4o-mini",
	Tools:        []*agents.FunctionTool{weather},
}
```

The most common fields:

| Field | Purpose |
|---|---|
| `Name` | Identifies the agent (required) |
| `Instructions` | The system prompt (a.k.a. developer message) |
| `Model` / `ModelImpl` | Model name resolved via the run's provider, or an explicit `Model` implementation |
| `ModelSettings` | Temperature, tool_choice, max tokens, … ([Models](models.md)) |
| `Tools` | Function tools the model may call ([Tools](tools.md)) |
| `MCPServers` | MCP servers whose tools are exposed to the agent ([MCP](mcp.md)) |
| `Handoffs` | Agents this agent can delegate to ([Handoffs](handoffs.md)) |
| `Guardrails` | Validation that can stop the run, one list across all stages ([Guardrails](guardrails.md)) |
| `OutputType` | Structured output schema (below) |
| `OnStart` / `OnEnd` | Callbacks around this agent's turn |

## Dynamic instructions

`Instructions` is an interface, so the system prompt can be computed per run from the [context](context.md):

```go
agent.Instructions = agents.InstructionsFunc(
	func(ctx context.Context, rc *agents.RunContext, a *agents.Agent) (string, error) {
		user := rc.Context.(*MyAppContext)
		return "The user's name is " + user.Name + ". Help them with their questions.", nil
	})
```

`agents.StaticInstructions("...")` wraps the fixed-string case.

## Stored prompts

Instead of (or alongside) inline `Instructions`, an agent can reference an [OpenAI stored prompt](https://platform.openai.com/docs/guides/prompting) via `Agent.Prompt`. The prompt's id, optional version, and variable substitutions are sent as the Responses API `prompt` parameter:

```go
agent.Prompt = agents.StaticPrompt(agents.Prompt{
	ID:        "pmpt_abc123",
	Version:   "2",                                   // optional
	Variables: map[string]any{"tone": "concise"},     // optional string substitutions
})
```

`agents.PromptFunc(func(ctx, rc, agent) (*agents.Prompt, error))` computes the prompt per run from the [context](context.md) — the counterpart of Python's `DynamicPromptFunction`. Only the OpenAI Responses backend honors `Prompt`; other backends ignore it. This is distinct from MCP server prompts (`server.GetPrompt`), which fetch prompt *text* to use as instructions.

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
- Schema generation failures (recursive types, `map` roots in strict mode) fail the run with a `*UserError` before any model call.

## Stopping after tools run

By default a turn's tool results go back to the model for another turn. Two
things override that, at two different levels.

A **tool** can end the run on its own result:

```go
func save(ctx context.Context, tc *agents.ToolContext, a Args) (agents.ToolResult, error) {
	r := agents.TextResult("saved")
	r.Terminate = true       // honored only if EVERY tool in the batch agrees
	return r, nil
}
```

A **run** can end at any turn boundary, deciding from what the turn produced:

```go
opts.Exec.ShouldStopAfterTurn = func(ctx context.Context, tr *agents.TurnResult) (bool, error) {
	return slices.Contains(tr.ToolCallNames(), "save_report"), nil
}
```

The hook fires after the turn's items are persisted and before the next model
call, so a run stopped here has its full history saved. It is a predicate, not
a producer: the final output is the turn's last message, or its last tool
output when the turn produced no message. Anything more specific is on
`RunResult.NewItems`.

This replaces the agent-level `ToolUseBehavior`. Deciding from what a turn
actually produced covers everything naming tools up front could, and the policy
belongs to the run rather than the agent — the same agent gets reused across
runs that want to stop at different points.

To prevent infinite tool loops, once an agent has called a tool the runner leaves `tool_choice` unset on its later turns (so a `"required"` or specific-tool setting cannot loop forever). Set `Agent.DisableToolChoiceReset = true` to keep `tool_choice` as configured on every turn — the inverse of Python's `reset_tool_choice=True` default.

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

Everything else the SDK used to expose as lifecycle hooks now has a better home:

| Was | Now | Difference |
|---|---|---|
| `OnAgentStart` / `OnHandoff` | `*AgentUpdatedStreamEvent` on the stream | — |
| `OnToolStart` / `OnToolEnd` | `Guardrail{Stages: StageToolInput/StageToolOutput}` | Can **rewrite**, not only refuse |
| `OnLLMStart` / `OnLLMEnd` | `RunOptions.Model.InputFilter`, `*RawResponsesStreamEvent` | Filter can **rewrite** |
| `OnAgentEnd` (run-scoped) | `*RunCompletedEvent` | — |

The eight-method interfaces, their empty base structs and the composite wrapper
are gone. What replaced them can do strictly more.

## Cloning

`Clone` returns a shallow copy — replace (rather than append to) slices when customizing:

```go
pirate := agent.Clone()
pirate.Name = "Pirate"
pirate.Instructions = agents.StaticInstructions("Talk like a pirate.")
```
