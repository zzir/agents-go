# Differences from the Python SDK

`agents-go` tracks [openai-agents-python](https://github.com/openai/openai-agents-python) v0.17.4: the run loop, item model, defaults (max turns 10, strict schemas on, tool errors fed back to the model, `tool_choice` reset after tool use) and most names map one-to-one. This page lists everything that intentionally differs — first how the same concepts look in Go, then what each side has that the other lacks.

## API mapping

| Python | Go |
|---|---|
| `Agent(name=..., instructions=...)` | `&agents.Agent{Name: ..., Instructions: agents.StaticInstructions(...)}` |
| `instructions=` callable | `agents.InstructionsFunc(func(ctx, rc, agent) (string, error))` |
| `Runner.run` / `Runner.run_sync` | `agents.Run(ctx, agent, input, opts)` (Go has no sync/async split) |
| `Runner.run_streamed` | `agents.RunStreamed(ctx, agent, input, opts)` |
| `run_config` / `Runner.run(...)` kwargs | `agents.RunOptions{...}` |
| `@function_tool` decorator | `agents.NewFunctionTool[Args, Result](name, desc, fn)` |
| pydantic argument model + docstring | argument struct + `json:"..."`/`jsonschema:"..."` tags |
| `output_type=MyModel` | `OutputType: agents.OutputType[MyModel]()` |
| `result.final_output_as(T)` | `agents.FinalOutputAs[T](res)` |
| `handoff(agent)` / `agent.handoffs` | `agents.HandoffTo(agent)` / `Agent.Handoffs` |
| `agent.as_tool(...)` | `agent.AsTool(agents.AgentToolConfig{...})` |
| `@input_guardrail` / `@output_guardrail` | `agents.InputGuardrail{Name, Run}` / `agents.OutputGuardrail{Name, Run}` struct values |
| `RunContextWrapper[T]` | `*agents.RunContext` with `Context any` (type-assert back) |
| `SQLiteSession` | `memory.FileSession` (JSONL file; same `Session` interface) |
| `reset_tool_choice=True` (default) | `DisableToolChoiceReset` (zero value = Python's default behavior) |
| `max_turns=10` | `RunOptions.MaxTurns` (0 means the same default of 10) |
| exceptions (`MaxTurnsExceeded`, …) | error values (`*MaxTurnsError`, …) matched with `errors.As` |
| `RunErrorDetails` on exceptions | `AgentsError.Details`, reachable via `agents.AsAgentsError(err)` |
| `set_default_openai_key` / globals | none — pass `openai.NewProvider(...)` explicitly in `RunOptions` |

## Language-level differences

**Generics and reflection instead of pydantic.** Tool schemas come from struct reflection at construction time (`NewFunctionTool[A, R]`), structured outputs from `OutputType[T]()`. Validation on the way back in uses `encoding/json` plus a root-level required-key check — looser than pydantic's full validation (nested required fields are not enforced).

**Two contexts instead of one wrapper.** Python's `RunContextWrapper[T]` carries both your data and run state. Go splits them: `context.Context` handles cancellation/deadlines (and is honored mid-run, mid-stream and inside tools), while `RunContext.Context any` carries your data without generics on every type.

**Errors instead of exceptions.** Every failure is a returned `error`. SDK error types embed `AgentsError`; `errors.As` matches concrete types even through `%w` wrapping, and `agents.AsAgentsError` extracts the embedded base (with `RunErrorDetails`) generically.

**Concurrency is explicit.** Tools requested in one turn run concurrently via goroutines (Python interleaves on the event loop). Hooks and shared context values must be goroutine-safe. Streaming uses `iter.Seq2` (`for event, err := range sr.Events()`) instead of `async for`, and there is no `run_sync` because `Run` is already synchronous.

**Sealed interfaces instead of unions.** `Tool`, `StreamEvent`, `RunItem` and `ToolUseBehavior` are closed interfaces you type-switch on, mirroring Python's `Union` types.

## Behavioral differences

| Area | Python v0.17.4 | Go |
|---|---|---|
| Tool errors | `failure_error_function` default feeds the error to the model | Same default (`DefaultToolErrorFunction`); set the field to `nil` for fatal |
| Tool timeout | — | `FunctionTool.Timeout` → `*ToolTimeoutError` |
| Model refusal | refusal text surfaces as plain content | run fails with `*ModelRefusalError` carrying the refusal |
| Handoff input filter | receives `input_history` / `pre_handoff_items` / `new_items` separately | receives one flattened `InputHistory`; the session always keeps the unfiltered conversation. `NestHandoffHistory` ports `nest_handoff_history` (fold + flatten) on top of this |
| HITL state | `RunState` JSON (Python format) | `RunState` JSON round-trips **Go↔Go only**, and rebuilding needs an agent-name registry (Go functions don't serialize) |
| Input guardrail timing | parallel with the first model call | same for `Run`; `RunStreamed` runs them synchronously before the first call |
| Streamed text items | `message_output_created` fires once per completed message | same (use raw delta events for token-level UI) |
| Session backends | SQLite / SQLAlchemy / Redis / encrypted | `InMemorySession` + `FileSession` (JSONL) in core; `sessions` module adds SQLite/PostgreSQL via bun; implement `Session` for anything else |
| Tracing backend | OpenAI traces dashboard by default | generic tracer → processor → exporter pipeline (console/HTTP/custom); **not** the OpenAI dashboard wire format. Traces export at start, spans at finish |
| Server-side conversation state | `previous_response_id` / `conversation_id` parameters | opt-in `RunOptions.UsePreviousResponseID`; `conversation_id` not wired up |
| Usage of nested `as_tool` runs | separate from parent | same (separate), but nested spans join the parent trace |

## Not implemented in Go

- **Hosted OpenAI tools**: web search, file search, code interpreter, computer use, image generation — deliberately not modeled; tools are provider-agnostic function tools, and a non-standard `tool_choice` is sent as a function name ([tools](tools.md))
- **Chat Completions model layer** — only the Responses API (use a Responses-compatible gateway, or implement `Model`)
- **LiteLLM adapter** — but native multi-provider routing, retry and fallback are supported via `Model` decorators ([models](models.md#retries-fallback-and-multiple-providers))
- **Structured / multimodal tool output** — a tool's result goes back to the model as text (JSON for non-string values); there is no `ToolOutputImage`/`ToolOutputText` equivalent, so a tool cannot hand the model native image input. This is also why MCP multimodal results (images/embedded resources) are JSON-encoded into text rather than passed as image input
- **Server-managed / compaction sessions** — `OpenAIConversationsSession` and history-compaction backends are not ported; Go offers in-memory, JSONL (`FileSession`) and SQL (`sessions` module) persistence plus `RunOptions.UsePreviousResponseID` for server-side chaining
- **Prompts API** — binding an agent's instructions to an OpenAI stored prompt (`prompt` id/version/variables); MCP server prompts (`server.GetPrompt`) are a separate, supported feature
- **Realtime and voice agents**
- **REPL utility (`run_demo_loop`) and visualization (Graphviz)**
- **Usage limits / cost estimation helpers**

## Go-only additions

- **[Sandboxes](sandbox.md)**: run model-written code in locked-down Docker containers or Kubernetes Jobs in your own infrastructure (`sandbox`, `sandbox/docker`, `sandbox/k8s` modules), exposed via `sandbox.CodeTool`
- **Per-tool `Timeout`** with a typed `ToolTimeoutError`
- **Hooks can veto**: any hook returning an error aborts the run (Python hooks are observe-only)
- **`FileSession`**: zero-dependency JSONL persistence with per-path locking and atomic rewrites
