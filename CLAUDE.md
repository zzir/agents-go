# CLAUDE.md

## What this is

A Go SDK for building agents on the OpenAI Responses API. It began as a port of
[openai-agents-python](https://github.com/openai/openai-agents-python) and shares
its core concepts (agents, handoffs, guardrails, sessions), but **evolves
independently** — it no longer tracks upstream.

Behavior is specified in [docs/spec.md](docs/spec.md), not inherited.
[docs/migration_from_python.md](docs/migration_from_python.md) maps the two APIs
for users arriving from Python.

Module path: `github.com/zzir/agents-go` (NOT `goagents`, despite the local
directory name).

## Commands

Requires Go 1.26+.

```bash
./scripts/ci.sh                       # full CI locally: gofmt, vet, build, race tests, every submodule (GOWORK=off)
go test -race ./...                   # race detector is ON in CI — keep it green
go test -race ./agents -run TestName  # single test
golangci-lint run                     # CI uses golangci-lint v2.12
```

## Layout

Go workspace (`go.work`, gitignored) with six modules. **A submodule exists only
to keep a heavy dependency out of the core** ([spec.md §5.7](docs/spec.md)) —
anything dependency-free stays in the root module. Non-root modules `require` the
root via `replace => ..`:

- **root** — the SDK
- **`sandbox/docker`**, **`sandbox/ssh`** — sandbox backends
- **`sessions`** — SQLite/PostgreSQL `Session` backends
- **`skills`** — Agent Skills (`SKILL.md`) loader
- **`cmd/agents-server`** — web app (REST + WS + embedded UI)

CI builds each module standalone with `GOWORK=off`, so a workspace-only fix can
hide a missing `go.mod` require — always validate with `./scripts/ci.sh`.

## Architecture

Core type: `agents.Agent` (a plain struct); everything orbits the runner.

- **Run loop** — `agents/run.go`, `run_step.go`. `RunStreamed`
  (`stream_run.go`) shares the same loop (`runner.sr != nil` gates the
  streaming-only bits), so run-semantics changes are written once in `run.go`.
- **Models** — `agents/model.go`; the backend is `models/openai`
  (Responses API). Retry / fallback / routing are provider-agnostic decorators
  (`NewRetryModel`, `NewFallbackModel`, `RouterProvider`) — never run-loop
  changes.
- **Tools** — `agents/function_tool.go`: `NewFunctionTool[Args, Result]`
  reflects Args into a strict-mode JSON schema. `agent.AsTool(...)` wraps an
  agent as a callable tool.
- **Handoffs / guardrails / HITL** — `handoff.go`, `guardrail.go` +
  `tool_guardrails.go`, `run_state.go`: `NeedsApproval` returns interruptions;
  serialize `RunState`, then `Approve`/`Reject` + `agents.ResumeRun` — runs
  survive process restarts.
- **Sessions** — `InMemorySession` / `memory.FileSession` in core; `sessions`
  module adds SQL backends; the `openai` package adds server-side variants
  (Conversations, Compaction, `UsePreviousResponseID` / `ConversationID`).
- **Tracing** — `tracing/`: a span is a `Type` tag + `Data map[string]any`,
  the idiomatic-Go stand-in for Python's typed SpanData.
- **MCP** — `mcp/`, bridged into the runner by `agents/mcp.go`.
- **Sandbox** — `sandbox/`: `Sandbox` interface, wrapped as a tool via
  `sandbox/tool.go`.

## Design decisions (deliberate — don't "fix" without cause)

The full list, with reasons, lives in [docs/spec.md](docs/spec.md) §1.2 (non-goals),
§3 (capabilities not provided) and §5 (recorded decisions). The two that come up most:

- **Responses API only.** Chat Completions is intentionally NOT supported and
  will not be ported. Internal item types are Responses types.
- **No hosted tools.** Every `Tool` is a locally-executed `FunctionTool`; the
  `Tool` interface is sealed. Provider-hosted tools (`web_search`,
  `file_search`, …) are deliberately not modeled — do not reintroduce them.

## Conventions

- **Behavior is specified, not inherited.** Answer behavior questions from
  `docs/spec.md`. When it does not cover a case: decide, implement, and add the
  invariant to `spec.md` **in the same change**. Open questions live in
  `spec.md` §6 — implementing one means moving it out of §6 first.
- **Upstream watch, not upstream parity.** After each upstream minor release,
  review its changelog and record the decision (ported / declined + why) in
  `docs/upstream_watch.md`. There is no obligation to match.
- **Docs track the code.** Any functional change must update the relevant
  `docs/` page — and `README.md` when it affects the feature set or
  quick-start. New public capabilities get a runnable example under
  `examples/`.
