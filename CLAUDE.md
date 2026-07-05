# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

A Go port of [openai-agents-python](https://github.com/openai/openai-agents-python)
(tracking **v0.17.7**). Goal: behavioral parity with idiomatic Go APIs. The Python
source is the reference spec — match its semantics. Intentional divergences and
open gaps are cataloged in [docs/python_differences.md](docs/python_differences.md);
keep it current when you close or open a gap.

Module path: `github.com/zzir/agents-go` (NOT `goagents`, despite the local
directory name).

## Commands

Requires Go 1.26+.

```bash
./scripts/ci.sh    # full CI locally: gofmt, vet, build, race tests, every submodule (GOWORK=off)
docker run --rm -v "$PWD":/src -w /src golang:1.26 ./scripts/ci.sh   # exact CI OS match

gofmt -l .                            # must print nothing; CI fails otherwise
go vet ./... && go build ./...
go test -race ./...                   # race detector is ON in CI — keep it green
go test -race ./agents -run TestName  # single test

# Submodules are separate Go modules — test each standalone
(cd sandbox/docker && go vet ./... && go test ./...)
(cd sandbox/ssh && go vet ./... && go test ./...)

golangci-lint run                     # CI uses golangci-lint v2.12
```

## Multi-module layout

Go **workspace** (`go.work`, gitignored) with six modules. Non-root modules exist
to keep heavy deps out of the core dependency graph; each `require`s the root
module with `replace => ..`:

- **root** — the SDK. Deps: `openai-go`, `jsonschema-go`, `go-sdk` (MCP), `x/sync`.
- **`sandbox/docker`** — Docker sandbox backend.
- **`sandbox/ssh`** — remote SSH sandbox backend (files via SFTP; no isolation/limits).
- **`sessions`** — SQLite/PostgreSQL `Session` backends via uptrace/bun.
- **`skills`** — Agent Skills (`SKILL.md`) loader; keeps the YAML dep out of core.
- **`cmd/agents-server`** — demo web app (REST + WS + embedded UI).

CI builds each module standalone with `GOWORK=off`, so a workspace-only fix can
hide a missing `go.mod` require — always validate with `./scripts/ci.sh`.

## Architecture

Core type: `agents.Agent` (a plain struct — instructions, model name, tools,
handoffs, guardrails, hooks, output type). Everything orbits the **runner**.

**Run loop** (`agents/run.go`, `run_step.go`): `agents.Run` → `runner.loop`. Each
turn: resolve model + settings (run-level over agent) → gather enabled tools
(expands MCP servers, evaluates per-tool predicates) → `Model.GetResponse` →
`processModelResponse` (classifies: tool calls / handoffs / final message) →
`executeToolsAndSideEffects` (concurrent tools, tool guardrails, HITL approval,
`ToolUseBehavior`) → next step: loop again, hand off, or finish.
`RunStreamed` (`stream_run.go`) shares the same loop — `runner.sr != nil` gates
the streaming-only bits (event emission, `streamOneModelCall`, synchronous input
guardrails), so run-semantics changes are written once in `run.go`.

**Models** (`agents/model.go`, `models/openai/`): `Model` = `GetResponse` +
`StreamResponse` (`iter.Seq2`); `ModelProvider.GetModel(name)` resolves names.
The backend is `models/openai` (Responses API); `TResponseInputItem` and friends
alias `openai-go/v3/responses` types. Retry / fallback / routing are
provider-agnostic **decorators** (`NewRetryModel`, `NewFallbackModel`,
`RouterProvider`) — never run-loop changes. OpenAI-specific error classification
lives in the provider (`openai.RetryableError` / `RetryAfter`). Streaming
retry/fallback can only switch backends before the first event is emitted.

**Tools** (`agents/function_tool.go`): `NewFunctionTool[Args, Result]` reflects
the Args struct into a strict-mode JSON schema (`schema.go`, `strict_schema.go`).
Tools carry optional `NeedsApproval`, timeout, error function, tool guardrails.
`agent.AsTool(...)` wraps an agent as a callable tool (nested run).

**Handoffs / guardrails / HITL**: `handoff.go` — control transfer between agents;
`InputFilter` rewrites the target's view, `NestHandoffHistory` folds prior
history into one summary. `guardrail.go` + `tool_guardrails.go` — run-level and
tool-level; a tripwire halts the run. `run_state.go` — `NeedsApproval` makes the
run return interruptions; serialize `RunState`, later `Approve`/`Reject` +
`agents.ResumeRun`. This is how runs survive process restarts.

**Sessions**: `InMemorySession` and `memory.FileSession` (JSONL) in core; the
`sessions` module adds SQLite/PostgreSQL; `openai.ConversationsSession` stores
history server-side; `openai.CompactionSession` decorates any Session with
`responses.compact` summarization (`CompactionAwareSession`, triggered by the
runner after saving); `SlidingWindowSession` compacts locally at pair-safe split
points. `UsePreviousResponseID` / `ConversationID` opt into server-side state
chaining (send only deltas; not combinable with a local Session). `Agent.Prompt`
binds an OpenAI stored prompt.

**Tracing** (`tracing/`): `Trace`/`Span` with `Processor`/`Exporter`. A span is a
`Type` tag (`SpanType*` constants via typed constructors) + `Data map[string]any`
— the idiomatic-Go stand-in for Python's typed SpanData. `StartSpan` for custom
spans.

**MCP** (`mcp/`): stdio / StreamableHTTP (SSE deprecated) servers as tool
sources, bridged into the runner by `agents/mcp.go`.

**Sandbox** (`sandbox/`): `Sandbox` interface (Local in core + docker/ssh
submodules), wrapped as a tool via `sandbox/tool.go`.

## Design decisions (deliberate — don't "fix" without cause)

- **Responses API only.** Chat Completions is intentionally NOT supported and
  will not be ported. Internal item types are Responses types — assume Responses
  everywhere.
- **No hosted tools.** Every `Tool` is a locally-executed `FunctionTool`; the
  `Tool` interface is sealed (unexported `isTool()`). Provider-hosted tools
  (`web_search`, `file_search`, `code_interpreter`, …) are deliberately not
  modeled — they couple a tool to one backend and have no client-side impl. This
  diverges from Python; do not reintroduce them.

## Conventions

- **Match upstream semantics.** Behavior questions are answered by the Python
  SDK, not invented. Check `docs/python_differences.md` before assuming a gap is
  intentional.
- **Docs mirror the Python docs 1:1** (`docs/`). New public capabilities get a
  runnable example under `examples/` and a doc page. Any functional change must
  update the relevant `docs/` page — and `README.md` when it affects the feature
  set or quick-start. A change is not complete until docs reflect it.
- Keep `go test -race ./...` green and everything `gofmt`-ed — CI is strict on both.
