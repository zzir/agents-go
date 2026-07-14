# CLAUDE.md

## What this is

A Go port of [openai-agents-python](https://github.com/openai/openai-agents-python),
tracking **v0.18.2**. The Python source is the reference spec — match its
semantics. Intentional divergences and open gaps are cataloged in
[docs/python_differences.md](docs/python_differences.md); keep it current.

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

Go workspace (`go.work`, gitignored) with six modules; non-root modules keep
heavy deps out of core and `require` the root via `replace => ..`:

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

- **Responses API only.** Chat Completions is intentionally NOT supported and
  will not be ported. Internal item types are Responses types.
- **No hosted tools.** Every `Tool` is a locally-executed `FunctionTool`; the
  `Tool` interface is sealed. Provider-hosted tools (`web_search`,
  `file_search`, …) are deliberately not modeled — do not reintroduce them.

## Conventions

- **Match upstream semantics.** Behavior questions are answered by the Python
  SDK, not invented; check `docs/python_differences.md` before assuming a gap
  is intentional.
- **Docs mirror the Python docs 1:1** (`docs/`). Any functional change must
  update the relevant `docs/` page — and `README.md` when it affects the
  feature set or quick-start. New public capabilities get a runnable example
  under `examples/`.
