# Feature reference

A one-line map from every capability to its API. Each section links to the doc
page with the full story.

## Core

See [Agents](agents.md), [Running agents](running_agents.md), [Tools](tools.md),
[Guardrails](guardrails.md), [Human-in-the-loop](human_in_the_loop.md).

| Capability | API |
|---|---|
| Agents | `agents.Agent{...}` |
| Run (blocking) | `agents.Run(ctx, agent, input, opts)` |
| Streaming | `agents.RunStreamed(...)` → `Events()` iterator |
| Function tools | `agents.NewFunctionTool[Args, Result](name, desc, fn)` |
| Structured output | `agents.OutputType[T]()` |
| Dynamic output schema | `agents.NewDynamicOutputSchema(name, schema, strict)` (runtime JSON Schema) |
| Multimodal tool output | `agents.ToolOutputText/ToolOutputImage/ToolOutputFile` |
| Handoffs | `agents.HandoffTo(targetAgent)` |
| Agent as tool | `agent.AsTool(agents.AgentToolConfig{...})`; typed params via `agents.AgentAsTool[Params](agent, cfg)` (validated, structured input rendering) |
| Guardrails | One `Guardrail` type across four `Stage`s (input / output / tool input / tool output), on the run, an agent or a single tool (incl. pre-approval via `RunOptions.PreApprovalToolInputGuardrails`) |
| Human-in-the-loop | `tool.NeedsApproval`, `RunState.Approve/Reject`, `agents.ResumeRun` |
| Error recovery | `RunOptions.ErrorHandlers` (fallback final output on max-turns / refusal / invalid structured output) |
| SDK-only tool output metadata | `FunctionTool.CustomDataExtractor` → `ToolCallOutputItem.CustomData` (never sent to the model) |
| Instruction composition | `agents.WrapInstructions(inner, prefix, suffix)` |
| Composite hooks | `agents.CompositeRunHooks(hooks...)` |

## Sessions & state

See [Sessions](sessions.md) and [Context management](context.md).

| Capability | API |
|---|---|
| Sessions | `agents.Session`, `InMemorySession`, `memory.FileSession` (JSONL), `sessions` module (SQLite/Postgres) |
| Server-side sessions | `openai.ConversationsSession` (OpenAI Conversations API) |
| History compaction | `openai.CompactionSession` (server-side `responses.compact`), `agents.NewSlidingWindowSession` (local summarize) |
| Session forking | `agents.ForkSession` / `ForkSessionAt` / `IndexOfItemID` |
| Server-side state | `RunOptions.UsePreviousResponseID` / `RunOptions.ConversationID` |
| Stored prompts | `Agent.Prompt = agents.StaticPrompt(...)` / `agents.PromptFunc(...)` |

## Reliability & routing

See [Models](models.md).

| Capability | API |
|---|---|
| Retry | `agents.NewRetryModel(...)` / `agents.NewRetryProvider(...)` (backoff + jitter) |
| Fallback | `agents.NewFallbackModel(...)` / `agents.NewFallbackProvider(...)` (try backends in order) |
| Multi-provider routing | `agents.NewRouterProvider(...)` (per-agent backend by model-name prefix) |

## Integrations

See [Tracing](tracing.md), [MCP](mcp.md), [Sandbox agents](sandbox.md),
[Skills](skills.md).

| Capability | API |
|---|---|
| Tracing | `tracing.NewTracer`, `tracing.NewBatchProcessor`; `RunOptions.TraceGroupID/TraceMetadata` |
| MCP | `mcp.NewStdioServer / NewStreamableHTTPServer` (`NewSSEServer` deprecated) |
| Sandbox (code execution) | `sandbox.CodeTool` + Local / Docker / SSH backends |
| Web search | `bravesearch.New(bravesearch.Options{...})` (Brave Search API) |
| File editing | `sandbox.ApplyPatchTool` (Codex-style patches, edits through the sandbox) |
| Skills | `skills.Load / LoadRecursive / RenderIndex / ReadFileTool` (Agent Skills `SKILL.md`) |

## Packages

Core module path: `github.com/zzir/agents-go`.

| Package | What it is |
|---|---|
| `agents` | Core: agents, runner, tools, guardrails, sessions, HITL, tracing hooks |
| `models/openai` | OpenAI Responses API model provider (built on `openai-go` v3) |
| `memory` | `FileSession` (JSONL file store, zero dependencies) |
| `tracing` | Traces, spans, processors and exporters |
| `mcp` | Model Context Protocol client |
| `sandbox` | `Sandbox` interface + `CodeTool` + `apply_patch` + local backend |
| `tools/bravesearch` | Brave Search web-search tool |
| `sandbox/docker` | **separate module** — Docker sandbox backend |
| `sandbox/ssh` | **separate module** — remote SSH sandbox backend |
| `sessions` | **separate module** — SQLite/PostgreSQL session store (uptrace/bun) |
| `skills` | **separate module** — Agent Skills (`SKILL.md`) loader |
| `cmd/agents-server` | **separate module** — web app (REST + WS + UI) |

### Layout notes

The core lives in a single `agents/` package. The original plan split it further
into `tools/`, `outputs/` and `models/`, but in Go those would form an import
cycle with the core (tool callbacks reference `RunContext`; the `Model` interface
references `Tool`), so they are kept together in `agents/`. Provider, storage,
tracing and MCP implementations live in subpackages that import `agents`. Items
use the `openai-go` Responses types as the wire format, mirroring how the Python
SDK reuses the OpenAI SDK types.

### Roadmap

The Responses **WebSocket transport** and a `Model` connection-lifecycle hook
(`Close`/`aclose`) are not implemented — only the HTTP Responses transport is
supported today. Track this if you need streaming over WebSocket.
