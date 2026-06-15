# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go port of [openai-agents-python](https://github.com/openai/openai-agents-python),
tracking a specific upstream version (currently **v0.17.4**, README says so; the
port progress memo tracks v0.17.5 deltas). The goal is behavioral parity with the
Python SDK using idiomatic Go APIs. When adding features, the Python source is the
reference spec — match its semantics. [docs/python_differences.md](docs/python_differences.md)
catalogs intentional divergences and current gaps; keep it current when you close or open a gap.

Module path is `github.com/zzir/agents-go` (note: NOT `goagents`, despite the
local directory name).

## Commands

```bash
# Run the full CI suite locally (formatting, vet, build, race tests, sandbox submodules).
# Sets GOWORK=off so each module is built standalone exactly as CI does.
./scripts/ci.sh

# For an exact OS match with CI (Linux):
docker run --rm -v "$PWD":/src -w /src golang:1.26 ./scripts/ci.sh

# Individual checks
gofmt -l .                      # must print nothing; CI fails on any unformatted file
go vet ./...
go build ./...
go test -race ./...             # race detector is ON in CI — keep it green

# Run a single test / package
go test -race ./agents -run TestToolOutputGuardrailRaise
go test -race ./agents/...

# Sandbox backends are SEPARATE Go modules — test them on their own
(cd sandbox/docker && go vet ./... && go test ./...)
(cd sandbox/k8s && go vet ./... && go test ./...)

# Lint (CI uses golangci-lint v2.12)
golangci-lint run
```

Requires Go 1.26+.

## Multi-module layout

This is a Go **workspace** (`go.work`), not a single module. Three modules:

- **root** (`github.com/zzir/agents-go`) — the SDK. Depends only on `openai-go`,
  `jsonschema-go`, `go-sdk` (MCP), `golang.org/x/sync`.
- **`sandbox/docker`** and **`sandbox/k8s`** — optional sandbox backends, each its
  own module so the heavy Docker/Kubernetes client deps don't leak into the root
  module's dependency graph. Anyone using only the core SDK pays nothing for them.

`go.work` is gitignored. CI builds each module standalone with `GOWORK=off` — so
**a workspace-only fix won't catch a missing `go.mod` require**. Always validate
with `./scripts/ci.sh` (which sets `GOWORK=off`) before assuming green.

## Architecture

The core type is `agents.Agent` (a plain struct — instructions, model name,
tools, handoffs, guardrails, hooks, output type). Everything orbits the **runner**.

### The run loop (`agents/run.go`, `agents/run_step.go`)

`agents.Run(ctx, agent, input, opts)` constructs an internal `runner` and calls
`runner.loop`. Each turn:

1. Resolve model (`resolveModel`) and settings (`resolveSettings`, run-level
   override merged over agent settings), gather enabled tools (`enabledTools`,
   which expands MCP servers and evaluates per-tool enable predicates).
2. Build the `ModelRequest` and call `Model.GetResponse`.
3. `processModelResponse` classifies the output into a `processedResponse`:
   function-tool calls, handoff calls, or a final message.
4. `executeToolsAndSideEffects` runs tools (concurrently), applies tool
   input/output guardrails, checks approval (HITL), evaluates `ToolUseBehavior`,
   and decides the `nextStepKind`: run the LLM again, hand off, or finish.

`stream_run.go` is the streaming counterpart (`RunStreamed` → `Events()`
iterator) and largely mirrors `run.go` — **changes to run semantics usually need
to land in both files.**

### Model abstraction (`agents/model.go`, `models/openai/`)

`Model` is a two-method interface: `GetResponse` (blocking) and `StreamResponse`
(an `iter.Seq2[*TResponseStreamEvent, error]`). `ModelProvider.GetModel(name)`
resolves an agent's model name to a `Model`. The only implementation today is
`models/openai` (Responses API). `convert.go` translates SDK types ↔ OpenAI
Responses API params; `responses_model.go` is the `Model` impl. The SDK speaks
the OpenAI **Responses** API format internally — `TResponseInputItem` and friends
are aliases of `openai-go/v3/responses` types.

### Tools (`agents/function_tool.go`, `agents/tool.go`)

`NewFunctionTool[Args, Result](name, desc, fn)` reflects the `Args` struct into a
JSON schema via `schema.go` + `strict_schema.go` (strict-mode normalization
required by the Responses API). Tools carry optional `NeedsApproval`, timeout,
error function, and input/output guardrails. `agent.AsTool(...)` wraps an agent
as a callable tool (nested run).

### Handoffs, guardrails, HITL

- **Handoffs** (`handoff.go`): an agent can transfer control to another agent.
  `InputFilter` can rewrite the conversation seen by the target.
- **Guardrails** (`guardrail.go`, `tool_guardrails.go`): input/output guardrails
  at the run level and the tool level; a tripwire halts the run.
- **Human-in-the-loop** (`run_state.go`): when a tool `NeedsApproval`, the run
  returns interruptions instead of completing. Serialize `RunState`, later call
  `Approve`/`Reject` and `agents.ResumeRun` to continue. This is how runs survive
  process restarts.

### Sessions, tracing, MCP, sandbox

- **Sessions** (`agents/session.go`, `memory/`): conversation persistence.
  `InMemorySession` and `memory.FileSession` (JSONL). `UsePreviousResponseID`
  opts into server-side state chaining instead of resending history.
- **Tracing** (`tracing/`): `Trace`/`Span` with `Processor`/`Exporter`. Spans
  carry an untyped `Data map[string]any` (Python's typed SpanData is not ported).
- **MCP** (`mcp/mcp.go`): Stdio / SSE / StreamableHTTP MCP servers exposed as
  tool sources; `agents/mcp.go` bridges them into the runner's tool list.
- **Sandbox** (`sandbox/`): pluggable code-execution backends (Local + the Docker
  and K8s submodules) behind a `Sandbox` interface, wrapped as a tool via `tool.go`.

## Design decisions

These are deliberate and load-bearing — don't "fix" them without cause:

- **Responses API only.** `models/openai` targets the OpenAI **Responses** API
  exclusively. The older **Chat Completions** API is intentionally NOT supported
  and there is no plan to port it. The SDK's internal item types are Responses
  types — assume Responses everywhere.
- **Tools are provider-agnostic; no hosted tools.** Every `Tool` is a
  `FunctionTool` the SDK executes locally, so the same tool works against any
  backend. The SDK deliberately does **not** model provider-hosted/server-side
  tools (OpenAI's `web_search`, `file_search`, `code_interpreter`, etc.), because
  those couple a tool to one backend and have no client-side implementation to
  run. `convertTools` only emits function-tool params; the `Tool` interface is
  sealed (unexported `isTool()`) and `FunctionTool` is its only implementation.
  This is a divergence from the Python SDK, which ships hosted-tool types — do
  not reintroduce them.

## Conventions

- **Match upstream semantics.** Behavior questions are answered by the Python SDK,
  not invented. Port gaps are tracked in `docs/python_differences.md` and the
  memory file — consult them before assuming something is missing on purpose.
- **Docs mirror the Python docs.** Files in `docs/` correspond 1:1 to the Python
  SDK doc pages. Update the relevant doc when you change public API.
- **`docs/` and `examples/` are part of the deliverable**, not afterthoughts. New
  public capabilities should get a runnable example under `examples/` and a doc.
- **Keep docs and README in sync with behavior.** Any functional change — new or
  changed public API, tool, flag, or default — must update the relevant page in
  `docs/` and, when it affects the feature set or quick-start, `README.md`. A
  change is not complete until the docs and README reflect it.
- Keep `go test -race ./...` green and everything `gofmt`-ed — CI is strict on both.
