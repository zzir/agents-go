# Coming from the Python SDK

`agents-go` began as a port of [openai-agents-python](https://github.com/openai/openai-agents-python)
and still shares its core concepts — agents, handoffs, guardrails, sessions, the
run loop shape, and most names map one-to-one. It **no longer tracks** the Python
SDK: behavior is specified in [spec.md](spec.md) and the two evolve independently.

This page is a **migration guide for people arriving from the Python SDK**, not a
parity report. It maps the concepts, then lists the differences you will notice.
For what this SDK deliberately does not provide (and why), read
[spec.md §1.2 and §3](spec.md); for upstream changes we have reviewed, see
[upstream_watch.md](upstream_watch.md).

> The comparison below was written against Python SDK **v0.18.2**, the last
> version this project tracked. Later Python releases are not reflected here.

## API mapping

| Python | Go |
|---|---|
| `Agent(name=..., instructions=...)` | `&agents.Agent{Name: ..., Instructions: agents.StaticInstructions(...)}` |
| `instructions=` callable | `Agent.Instructions` is a func type: assign `func(ctx, rc, agent) (string, error)` directly |
| `Runner.run` / `Runner.run_sync` | `agents.RunSync(ctx, agent, input, opts)` (Go has no sync/async split) |
| `Runner.run_streamed` | `agents.Run(ctx, agent, input, opts)` → `(RunStream, RunControl)`. `agents.RunSync` is the non-streaming counterpart of `Runner.run` |
| `run_config` / `Runner.run(...)` kwargs | `agents.RunOptions{...}` |
| `@function_tool` decorator | `agents.NewTool[Args, Result](name, desc, fn)` |
| pydantic argument model + docstring | argument struct + `json:"..."`/`jsonschema:"..."` tags |
| `output_type=MyModel` | `OutputType: agents.OutputType[MyModel]()` |
| `ToolOutputText` / `ToolOutputImage` / `ToolOutputFileContent` | `agents.ToolOutputText` / `ToolOutputImage` / `ToolOutputFile` (return one, or `[]agents.ToolOutputContent`, from a function tool) |
| `result.final_output_as(T)` | `agents.FinalOutputAs[T](res)` |
| `handoff(agent)` / `agent.handoffs` | `agents.HandoffTo(agent)` / `Agent.Handoffs` |
| `agent.as_tool(...)` | `agent.AsTool(agents.AgentToolConfig{...})` |
| `@input_guardrail` / `@output_guardrail` | one `agents.Guardrail` type across all stages: `agents.NewInputGuardrail(name, fn)` / `agents.NewOutputGuardrail(name, fn)` |
| `RunContextWrapper[T]` | `*agents.RunContext` with `Context any` (type-assert back) |
| `SQLiteSession` | `memory.FileSession` (JSONL file; same `Session` interface) |
| `reset_tool_choice=True` (default) | `DisableToolChoiceReset` (zero value = Python's default behavior) |
| `max_turns=10` | `RunOptions.Exec.MaxTurns` (0 means the same default of 10) |
| exceptions (`MaxTurnsExceeded`, …) | error values (`*MaxTurnsError`, …) matched with `errors.As` |
| `RunErrorDetails` on exceptions | `RunError.Result` — a failed run's partial progress as a `*RunResult`, via `errors.AsType[*agents.RunError]` |
| `set_default_openai_key` / globals | none — pass `openai.NewProvider(...)` explicitly in `RunOptions` |
| `custom_data_extractor=` (function tools) | `ToolResult.Details` — the tool declares its UI data when it returns, instead of a second extraction pass ([tools](tools.md#returning-more-than-a-value-toolresult)) |
| `RunConfig.tool_execution.pre_approval_tool_input_guardrails` | `RunOptions.Exec.PreApprovalToolInputGuardrails` |
| resume a paused run (state as input to `Runner.run` / `Runner.run_streamed`) | `agents.ResumeRunSync(ctx, state, opts)` / `agents.ResumeRun(ctx, state, opts)` |
| `error_handlers={"max_turns": ..., "model_refusal": ..., "invalid_final_output": ...}` | `RunOptions.Exec.ErrorHandlers` struct (`MaxTurns` / `ModelRefusal` / `InvalidFinalOutput` fields); handlers return `(*RunErrorHandlerResult, error)` — `(nil, nil)` declines like Python's `None`; `include_in_history=True` default becomes the `ExcludeFromHistory` zero value |

## Language-level differences

**Generics and reflection instead of pydantic.** Tool schemas come from struct reflection at construction time (`NewTool[A, R]`), structured outputs from `OutputType[T]()`. Validation on the way back in is full JSON Schema validation (`google/jsonschema-go`, already a dependency for schema generation): nested `required`, nested type mismatches, enums and bounds are all enforced on structured outputs and tool arguments, and errors carry a JSON-pointer path the model can act on. Schema `default` values are applied before decoding. One deliberate relaxation: `additionalProperties: false` is sent to the provider but **not** enforced locally — an unexpected key is dropped by Go decoding and the tool cannot see it, so rejecting the call would turn a harmless extra into a failed turn, while a misspelled key is still caught by `required`. Two schema-shape limits are rejected by the Go reflector rather than becoming API 400s: `any`/`interface{}` fields (no strict-mode schema exists for "anything") and **recursive types** (pydantic emits `$defs`/`$ref` for these). `NewTool` panics on them at construction — the same moment Python raises at decoration time — while `NewRawTool`, whose schema is runtime data, returns an error instead.

**Two contexts instead of one wrapper.** Python's `RunContextWrapper[T]` carries both your data and run state. Go splits them: `context.Context` handles cancellation/deadlines (and is honored mid-run, mid-stream and inside tools), while `RunContext.Context any` carries your data without generics on every type.

**Errors instead of exceptions.** Every failure is a returned `error`. `errors.As` matches the concrete SDK error types even through `%w` wrapping; `agents.CodeOf` gives the transportable classification; a failed run's partial progress rides on `*agents.RunError` as a `*RunResult`.

**Concurrency is explicit.** Tools requested in one turn run concurrently via goroutines (Python interleaves on the event loop). Hooks and shared context values must be goroutine-safe. Streaming uses `iter.Seq2` (`for event, err := range stream`) instead of `async for`, and a run executes on the consumer's goroutine — ranging the stream advances the loop.

**Sealed interfaces instead of unions.** `Tool`, `StreamEvent` and `RunItem` are closed interfaces you type-switch on, mirroring Python's `Union` types.

## Behavioral differences

| Area | Python v0.18.2 | Go |
|---|---|---|
| `tool_use_behavior` | agent-level: `run_llm_again` / `stop_on_first_tool` / `StopAtTools` / a callable | **not ported.** `stop_on_first_tool` → the tool returns `ToolResult{Terminate: true}` (honored when the whole batch agrees, so a parallel tool's result is never discarded); everything else → `RunOptions.Exec.ShouldStopAfterTurn`, a run-level predicate over the finished `TurnResult`. The final output is derived from the turn rather than supplied by the callback, so it cannot disagree with the saved history |
| Tool errors | `failure_error_function` default feeds the error to the model | Same default (`DefaultToolErrorFunction`); set the field to `nil` for fatal |
| Tool timeout | `timeout_seconds` + `timeout_behavior` (`error_as_result` / `raise_exception`) | `Tool.Timeout` → `*ToolTimeoutError`, fed back via `FailureErrorFunction` when set (≈ `error_as_result`), else fatal (≈ `raise_exception`). Enforced by the runner: the call returns at the deadline even if the tool ignores its context (the tool goroutine finishes in the background, its late result discarded) |
| Tool panics | tool exceptions flow into `failure_error_function` | same: a panicking tool (or guardrail) is recovered and converted to an error instead of crashing the process |
| HITL interruption scope | tools not needing approval still execute in the interrupted turn; only approval-gated calls pause | **all** tool calls in the turn wait until `ResumeRun` when any of them needs approval (keeps `RunState` free of partial results; side effect: "safe" tools run with post-approval context) |
| Model refusal | raises `ModelRefusalError` (recoverable via `error_handlers`) | same: `*ModelRefusalError` carrying the refusal, recoverable via `RunOptions.ErrorHandlers.ModelRefusal` |
| Handoff input filter | receives `input_history` / `pre_handoff_items` / `new_items` separately | receives one flattened `InputHistory`; the session always keeps the unfiltered conversation. `NestHandoffHistory` ports `nest_handoff_history` (fold + flatten) on top of this |
| HITL state | `RunState` JSON (Python format) | `RunState` JSON round-trips **Go↔Go only**, and rebuilding needs an agent-name registry (Go functions don't serialize). The state carries `max_turns` so `ResumeRun` continues under the original budget; resumed `NewItems` deserialize as raw items (`ItemType()` survives, concrete type assertions don't) |
| Input guardrail timing | parallel with the **whole first turn** (model call + tool execution); a tripwire cancels the in-flight model task (not billed, no `on_llm_end`) | Overlapped with the model call only — tools never start before guardrails pass, and a tripwire does not cancel the in-flight call (it completes, is billed, and fires `OnLLMEnd` before the run aborts). Both entry points behave identically; `Blocking: true` gates instead of racing |
| Streamed text items | `message_output_created` fires once per completed message | same (use raw delta events for token-level UI) |
| Session backends | SQLite / SQLAlchemy / Redis / encrypted / OpenAI Conversations / compaction | `InMemorySession` + `FileSession` (JSONL) in core; `sessions` module adds SQLite/PostgreSQL via bun; `openai.ConversationsSession` (server-side via the Conversations API); `openai.CompactionSession` (`responses.compact` decorator, attempted once per run vs Python's per turn); implement `Session` for anything else |
| Tracing backend | OpenAI traces dashboard by default | generic tracer → processor → exporter pipeline (console/HTTP/custom); **not** the OpenAI dashboard wire format. Traces export at start, spans at finish |
| Sensitive trace data | `RunConfig.trace_include_sensitive_data` (env `OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA`) gates span content | `RunOptions.Observe.IncludeSensitiveData *bool`, same env var and default (true); gates the generation span's request/response data keys (`model`, `system_instructions`, `input`, `tools`, `model_settings`, `handoffs`, `output_schema`, `output`, …) and the function span's `input`/`output` |
| Compaction tracing | not traced | Go-only: the runner wraps `RunCompaction` in a `"compaction"` span, opened lazily via `CompactionArgs.StartSpan` so no-op passes emit nothing; sessions annotate before/after item counts |
| Compaction failure | raises, failing the run | best-effort: the run's items are already saved and the final output produced, so the error is recorded on the compaction span and the run still succeeds |
| MCP tool errors | an `isError` result's content passes to the model **verbatim** (never aborts); transport errors go through `failure_error_function` | same: an `isError` result's content passes through verbatim, transport errors are fed back via the tool's error function; strict-schema normalization failures silently fall back to non-strict for that tool (Python logs). Duplicate tool names across servers/local tools are a `UserError` on both sides |
| Server-side conversation state | `previous_response_id` / `conversation_id` parameters | `RunOptions.Conversation.UsePreviousResponseID` and `RunOptions.Conversation.ConversationID` (both send only deltas; neither combines with a local Session). `openai.ConversationsSession` also persists history server-side via the Conversations API |
| Default model & implicit settings | agent with no model → `gpt-5.4-mini` (env `OPENAI_DEFAULT_MODEL`); GPT-5 family models get implicit `reasoning.effort` + `verbosity="low"`; a stored-prompt request omits `model` / a named `tool_choice` so the prompt's pinned model applies | **not ported (intentional).** The OpenAI provider ships no built-in default model — name a model per agent (`Agent.Model`) or via `provider.WithDefaultModel(...)`, else `Model` returns a `*UserError`. No implicit GPT-5 reasoning/verbosity is injected and no stored-prompt model/tool omission is applied; callers set `Agent.ModelSettings` (and the model) explicitly. Keeps the Go API small and predictable |
| Stored prompts | `Agent(prompt=Prompt(id, version, variables))` / `DynamicPromptFunction` | `Agent.Prompt` = `StaticPrompt(agents.Prompt{...})` or a `func(ctx, rc, agent) (*agents.Prompt, error)` (OpenAI Responses backend only) |
| Usage of nested `as_tool` runs | accumulated into the parent run's usage (the nested run shares the parent's context wrapper) | same: a completed nested run's usage is folded into the parent run's `Usage`; nested spans also join the parent trace |
| Final-turn session save vs output guardrails | blocking runs save the final turn **before** output guardrails (a tripped run's session keeps the flagged message); streaming saves after | always after the guardrails pass (= Python's streaming order): a tripped final output is never persisted. `on_agent_end` hooks fire before output guardrails on both sides |
| Session content while paused for approval | the pending `function_call` items are persisted at the interruption (dangling calls are scrubbed on later reads) | deliberately held back (`safePersistBoundary`): the stored conversation never contains a call without its output; pending calls persist with their outputs after `ResumeRun` |
| Non-string tool outputs | stringified with Python `str()` (`{'a': 1}`, `True`, `None`) | JSON-encoded (`{"a":1}`, `true`, `""`) — Go has no `repr` equivalent, and JSON is the less ambiguous model-visible form |
| `RunItem` discriminator strings | `message_output_item`, `tool_call_item`, … (`_item` suffix) | `ItemType()` returns `message_output`, `tool_call`, … — Go names, stable within Go↔Go `RunState` round-trips |
| Streamed per-item event timing | `tool_called` / `reasoning_item_created` are emitted mid-stream as each `output_item.done` raw event arrives; message/tool-output events after tool execution | all model-output item events are emitted in one batch after the model response completes (before tool execution), side-effect events after |
| Pending approvals in results | `ToolApprovalItem` is a `RunItem` and appears in `new_items` at an interruption | approvals surface only in `RunResult.Interruptions`; `NewItems` holds just the model/tool items |
| `RunState` extras | serializes the run-context user data and trace state (a resume re-attaches the same trace) | carries neither: resume takes user data from `RunOptions` and starts a fresh trace (`"<workflow> (resumed)"`) |
| MCP call cancellation | typed `MCPToolCancellationError` | plain `context.Context` cancellation (`ctx.Err()`) |
| Session UI metadata | tool-call items may carry `_agents_tool_title`/`_agents_tool_description` keys (stripped before model calls) | sessions store pure API items; SDK-only metadata lives on `ToolCallOutputItem.Extra`, never persisted |
| Trace span granularity | one span per guardrail (named after it, with a `triggered` flag), `mcp_tools` spans per list call, `mcp_data` on function spans | one aggregate span per guardrail stage (`"input"` / `"output"`), no MCP list spans, no `mcp_data` |
| Generation span usage keys | per-call usage includes cached and cache-write input-token breakdowns | `input_tokens` / `output_tokens` / `total_tokens` only — the breakdowns live on `Usage.InputTokensDetails`, not on spans |
| Prompt cache key | may auto-generate a `prompt_cache_key` (sniffing the endpoint) and carry it across a resumed run | typed `ModelSettings.PromptCacheKey` field only — the runner never auto-generates one, sniffs the endpoint, or persists it in `RunState` ("Option A"); set it explicitly or via `ExtraBody["prompt_cache_key"]` |
| Stored-prompt variables | prompt variable values may be text or content (image/file) inputs | only string (text) values are supported; a non-string variable is rejected with a `*UserError` rather than silently stringified |
| `RunContext.TurnInput` | `turn_input` attribute | `TurnInput()` method (guarded, returns a copy): exactly what was sent to the model this turn, after session history, handoff filtering, compaction and `CallModelInputFilter`. Under `UsePreviousResponseID` / `ConversationID` only new items go on the wire, so that is what it reports |
| Reasoning-item id omit | `reasoning_item_id_policy="omit"` drops the `id` key entirely | `ReasoningItemIDOmit` blanks the reasoning id to `""` (openai-go always marshals the `id` key) rather than dropping it — only the stale value is removed |
| MCP client-side validation & naming | `_validate_required_parameters` raises; server-name prefix dedup runs through a shared cross-server manager | a missing required argument is a `*UserError` but, because MCP tools carry `DefaultToolErrorFunction`, it is fed back to the model rather than aborting; collisions across servers are avoided with an explicit `ToolNamePrefix` per server (no auto-renaming manager) |
| Guardrail default name | an unnamed guardrail falls back to the guardrail function's `__name__` | fixed labels `"input_guardrail"` / `"output_guardrail"` — Go has no function-name reflection |
| Unknown-tool model message | feeds `Tool 'X' not found.` back to the model | **same** — the model-visible text matches upstream verbatim (converged; no longer a wording divergence) |
| `RunState` schema version | its own state versioning | `RunStateSchemaVersion` is `"1.4"` (bumped for nested-state serialization, guardrail-result carriage, then pending input / disclosed tools / server cursor). Decoding accepts a window rather than strict equality: same major, no newer than that minor, no older than `runStateOldestDecodableMinor` — so once a bump only ADDS fields, a run paused before an SDK upgrade still resumes after it. The floor equals the current minor today because `"1.3"` was released with two different field shapes and the version string cannot tell them apart; the window opens at the next additive bump |
| Guardrail results across resume | `RunState` serializes input/output/tool guardrail results and re-seeds them on resume | same intent — carried on `RunState` and rehydrated so a resumed `RunResult` still reports them — but serialized **lossily**: the guardrail's live `Run` func does not round-trip, so a decoded result carries a name-only stub guardrail plus the output payload (`OutputInfo` via a JSON round-trip) |

## In Python, not here

Two kinds of entry are mixed below: **deliberate non-goals** (recorded in
[spec.md §1.2 / §3](spec.md) — they will not appear) and **things nobody has
needed yet** (open to contribution). Each entry says which it is.

- *(non-goal)* **Hosted OpenAI tools**: web search, file search, code interpreter, computer use, image generation, `local_shell`, `apply_patch` — deliberately not modeled; tools are provider-agnostic function tools, and a non-standard `tool_choice` is sent as a function name. (For file editing, Go provides `apply_patch` as a **sandbox-backed** function tool — Codex-style patches applied through the `Sandbox` abstraction, not the hosted OpenAI `apply_patch`; [tools](tools.md))
- *(non-goal)* **Chat Completions model layer** — only the Responses API (use a Responses-compatible gateway, or implement `Model`)
- *(non-goal)* **LiteLLM adapter** — but native multi-provider routing, retry and fallback are supported via `Model` decorators ([models](models.md#retries-fallback-and-multiple-providers))
- *(not yet)* **Redis / encrypted / SQLAlchemy session backends** — only SQLite & PostgreSQL are provided (`sessions` module); implement `Session` for others. (`OpenAIConversationsSession` and `OpenAIResponsesCompactionSession` **are** ported, as `openai.ConversationsSession` and `openai.CompactionSession`.)
- *(non-goal)* **Realtime and voice agents**
- *(non-goal)* **REPL utility (`run_demo_loop`) and visualization (Graphviz)**
- *(not yet)* **Responses-over-WebSocket transport** (`OpenAIResponsesWSModel`, `use_responses_websocket`) and the `Model.close()` / `ModelProvider.aclose()` / run-scoped `Model._cleanup_on_run_end` (v0.18) lifecycle hooks — HTTP only; a custom Go `Model` manages its own connections
- *(non-goal)* **Hosted multi-agent beta** (`OpenAIHostedMultiAgentModel`, v0.18.2 experimental) — server-side subagent orchestration over the Responses WebSocket; falls under both the no-hosted-tools and HTTP-only decisions above
- *(not yet)* **`agent.as_tool(previous_response_id=...)`** — the only as_tool option not ported: Go's `RunOptions` has no explicit response-id entry point (`UsePreviousResponseID` is an automatic bool chain). The rest of the surface exists on `AgentToolConfig` (`OnStream`, `IsEnabled`, `NeedsApproval`/`Func`, `FailureErrorFunction`, `InputBuilder` — with `AgentToolInputWithSchema` as `include_input_schema`) plus `AgentAsTool[Params]` for typed parameters (a free function — Go methods cannot take type parameters). Run-level options Python spreads across as_tool parameters (`max_turns`, `session`, `conversation_id`, `run_config`) all go through the one `ModifyRunOptions` channel. Builders return text only, not item lists
- *(not yet)* **MCP-level `custom_data_extractor`** — Python's MCP servers accept their own custom-data extractors with access to the raw `CallToolResult`; Go's MCP bridge reports `IsError` but does not yet let a caller project the raw result into `ToolResult.Details`
- *(non-goal)* **`ModelSettings.extra_args`** — the free-form request-passthrough dict is intentionally not ported: `ExtraBody` (with `ExtraHeaders` / `ExtraQuery`) already covers forwarding arbitrary fields to the provider request

## Beyond the Python SDK

- **Self-hosted [sandboxes](sandbox.md)**: run model-written code in your own infrastructure — locked-down Docker containers (`sandbox/docker`) or a remote host over SSH (`sandbox/ssh`) — exposed via `sandbox.CodeTool`. Python has since grown its own sandbox stack (self-hosted `docker` / `unix_local` in core plus hosted providers — e2b / daytona / cloudflare / runloop / vercel — as extensions) with a PTY session model; the Go `Sandbox` interface predates it and stays a deliberately smaller surface: Exec + file operations, no PTY sessions, no hosted providers, plus an SSH backend Python lacks
- **Hooks can veto**: any hook returning an error aborts the run (Python hooks are observe-only)
- **`FileSession`**: zero-dependency JSONL persistence with per-path locking and atomic rewrites
- **[Skills](skills.md)** (`skills` module): the open [Agent Skills](https://github.com/agentskills/agentskills) `SKILL.md` format implemented on `Instructions` + a function tool — provider-agnostic and sandbox-free, unlike Python's sandbox-capability skills
- **Session forking** (`ForkSession`): clone a conversation's active branch into another session — works across any `Session` backend pair (a point-in-time fork is `PathToLeaf` + `session.ReplaceEntries`). Python's closest is `AdvancedSQLiteSession`'s branch support, which is tied to that one backend
- **`AtomicReplacer` / `GuardedReplacer`**: optional storage capabilities for swapping the whole history in one step — unconditionally, or only while its highest sequence number is still the one the caller read. Only `openai.CompactionSession` needs them: the server-side compact API returns a replacement rather than a decision, with a network round trip between reading the history and writing it back. Local compaction is append-only ([Sessions](sessions.md#run-level-compaction))
- **Run-level compaction** (`RunOptions.Compaction` + `agents/compaction`): provider-agnostic, grouped so a tool call is never split from its output, triggered at three points including mid-run, and **append-only** — a checkpoint records what was folded instead of rewriting history
- **Provider-level decorators**: `NewRetryProvider(inner, policy)` and `NewFallbackProvider(primary, fallbacks...)` wrap a `ModelProvider` so every `Model` it produces automatically retries or falls back — the provider-level counterparts of `NewRetryModel` / `NewFallbackModel`, useful when you know the policy at configuration time but not the model name. Fallback error classification is configurable via `WithShouldFallback` (default: everything except context cancellation advances the chain)
- **Stream-only backend adaptation**: `NewStreamOnlyModel(inner)` / `NewStreamOnlyProvider(inner)` serve blocking `Respond` calls via an internal stream, for backends that reject non-streaming requests (e.g. the ChatGPT Codex backend). Python has no equivalent decorator
- **`NewDynamicOutputSchema`**: builds an `OutputSchema` from a `map[string]any` JSON Schema at runtime, complementing the compile-time `OutputType[T]()` for config-driven agents
- **`WrapInstructions`**: decorates an `Instructions` func with a prefix and/or suffix applied at resolution time — the inner instructions may themselves be computed per run, so joining has to happen then, not at wrapping
- **Run middleware** (`agents/middleware`): `Loop` re-runs until an evaluator accepts, `Approval` answers approval pauses from a standing policy, `Retry` re-runs a failed run, `Logging` records a run's shape. Python has no equivalent; it replaced the lifecycle-hook interfaces this SDK used to have
- **`RetryPolicy` JSON round-trip**: `RetryPolicy` implements `json.Unmarshaler` / `json.Marshaler` with millisecond-based fields (`base_delay_ms`, `max_delay_ms`), making it directly usable with `json.Unmarshal` from configuration stores
- **Simplified guardrail constructors**: `NewInputGuardrail(name, fn)` and `NewOutputGuardrail(name, fn)` accept a callback that receives only the input/output, skipping ctx/rc/agent when you don't need them
- **Session item helpers**: `MarshalItems` / `UnmarshalItems` handle the common JSON ↔ `[]InputItem` round-trip (including nil/empty/"null" edge cases) so DB session backends don't rewrite it
- **`NewRawTool`**: builds a `Tool` from a pre-built JSON Schema `map[string]any` and a raw-JSON callback, for tools whose schema is loaded at runtime rather than reflected from a Go type. It returns `(*Tool, error)` — a runtime schema is data, so a bad one is an error rather than the construction panic `NewTool` reserves for type bugs
- **Enum parse helpers**: `ParseToolNotFoundBehavior(string)` / `ToolNotFoundBehavior.String()` convert between configuration strings and SDK enum types
