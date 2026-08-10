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
go run ./cmd/verifyexamples           # every example still runs (fake model APIs)
go run ./cmd/verifydocs               # doc snippets + doc.go links name things that exist
golangci-lint run                     # CI uses golangci-lint v2.12
```

## Layout

Go workspace (`go.work`, gitignored) with twelve modules. **A submodule exists only
to keep a heavy dependency out of the core** ([spec.md §5.7](docs/spec.md)) —
anything dependency-free stays in the root module. Non-root modules `require` the
root via `replace => ..`:

- **root** — the SDK (includes `tools/bravesearch` and `models/modelkit`, the
  dependency-free toolkit + conformance suite for model adapters)
- **`mcp`** — MCP client and server (carries modelcontextprotocol/go-sdk and the
  seven indirect requirements that came with it; import path unchanged)
- **`models/anthropic`** — Anthropic Messages API backend (carries
  anthropic-sdk-go; translates to the canonical Responses format, spec §5.10)
- **`sandbox/docker`**, **`sandbox/ssh`** — sandbox backends
- **`sessions`** — SQLite/PostgreSQL `Session` backends
- **`skills`** — Agent Skills (`SKILL.md`) loader
- **`tracing/otel`** — OpenTelemetry exporter (the core stays vendor-neutral)
- **`cmd/agents-server`** — web app (REST + WS + embedded UI)
- **`examples/otel`**, **`examples/anthropic`**, **`examples/mcpserver`** — the
  examples with their own modules, for their extra deps

CI builds each module standalone with `GOWORK=off`, so a workspace-only fix can
hide a missing `go.mod` require — always validate with `./scripts/ci.sh`. The
reverse hides too: `go.work` is gitignored and CI never reads it, so a module
missing from the local `use` block drops out of `go test ./...` with no error at
all — after adding a module, run `go work use ./<module>`.

## Architecture

Core type: `agents.Agent` (a plain struct); everything orbits the runner.

- **Entry points** — `Run` returns `(RunStream, RunControl)`; `RunSync` returns
  `(*RunResult, error)`. Both go through `withMiddleware` → `runStream`, which
  takes a `rawEvents bool` for the only difference between them (whether the
  model call is streamed). Run-semantics changes are written once, in the
  `agents/run*.go` family: `run.go` holds the loop, `run_step.go` classifies a
  model response into the turn's work, `run_tools.go` executes tools (approval
  partition included), `run_handoff.go` executes handoffs, and the other
  `run_*.go` files one loop stage each (options, prepare, input guardrails,
  server cursor, persist, finish, resolve, tracing, error handlers).
- **A run executes on the consumer's goroutine.** Ranging the stream advances
  the loop; abandoning it stops the run. No producer goroutine, no context that
  must be cancelled on early exit.
- **Middleware** — `agents/middleware.go` defines `RunMiddleware`;
  `agents/middleware/` ships `Loop`, `Approval`, `Retry`, `Plan`, `Todo`.
  Wrapping a whole run belongs here, not in the loop.
- **Models** — `agents/model.go`; backends are `models/openai` (Responses
  API, the native format) and `models/anthropic` (Messages API, translated in
  the adapter — spec §5.10). `models/modelkit` holds the shared adapter
  plumbing and `modelkit/conformancetest` the golden matrix every `Model`
  implementation must pass. Retry / fallback / routing are provider-agnostic
  decorators (`NewRetryModel`, `NewFallbackModel`, `RouterProvider`) — never
  run-loop changes.
- **Tools** — `agents/function_tool.go`: `NewTool[Args, Result]`
  reflects Args into a strict-mode JSON schema. `agent.AsTool(...)` wraps an
  agent as a callable tool.
- **Handoffs / guardrails / HITL** — `handoff.go`, `guardrail.go` (one
  `Guardrail` type across four stages; the runner-side input gate/race lives
  in `run_input_guardrails.go`), `run_state.go`: `NeedsApproval` returns
  interruptions; serialize `RunState`, then `Approve`/`Reject` +
  `agents.ResumeRun` — runs survive process restarts.
- **Sessions** — the `agents/session` subpackage, three layers: `session.Storage`
  (reads/writes entries, knows no meaning), `session.Session` (a struct, turns
  entries into model input), and `session.Projector` (which kinds reach the
  model). The shared value types (`Source`, `ItemDisplay`, `RequestUsage`,
  `Diagnostic`, `ErrorCode`) live in session and are aliased in agents. Storage is
  `InMemoryStorage` / `filesession.Store` in core, SQL in the `sessions`
  module, server-side variants in `openai` (Conversations, Compaction,
  `UsePreviousResponseID` / `ConversationID`).
- **Entries are append-only** — `agents/session/entry.go`. A session is a TREE:
  `ParentID` links, `Branch`/`PathEntries` walk it, and a display that settles
  late is a new UPDATE entry folded in at read time, never a rewrite.
- **Compaction** — `agents/compaction_points.go` drives it at three points;
  `agents/compaction/` holds the strategies. A checkpoint is appended, never a
  rewrite; `agents/overflow.go` turns a context-overflow error into
  compact-and-retry.
- **Tracing** — `tracing/`: a span is a `Type` tag + `Data map[string]any`,
  rather than one Go type per span kind. The current parent
  travels on the `context`, so a subsystem outside the runner (retry, MCP,
  sandbox) opens a child without a threaded handle. `tracing/otel` maps it to
  OpenTelemetry.
- **MCP** — `mcp/`: a client bridged into the runner by `agents/mcp.go`, and
  `mcp/serve.go` for the other direction (expose tools or a whole agent as an
  MCP server). Its own module — `agents.MCPServer` is the inversion that keeps
  the go-sdk out of the core, so never import `mcp` from `agents`.
- **Sandbox** — `sandbox/`: `Sandbox` interface, wrapped as a tool via
  `sandbox/tool.go`; `policy.go` filters commands before the approval gate.
- **Background tasks** — `agents/tasks/`: sub-agents that outlive the turn that
  spawned them. The wake-up debt is a persisted state machine, and terminal
  state is claimed by compare-and-set.
- **Fan-out** — `agents/fanout.go`: one producer, many consumers, per-subscriber
  buffers. A dropped event is reported as a `*GapError`, never silent.
- **Test doubles** — `agentstest/` is the public harness for code that USES the
  SDK. The `agents` package cannot import it (cycle), which is why
  `agents/run_test.go` has its own unexported `fakeModel`.

## Design decisions (deliberate — don't "fix" without cause)

The full list, with reasons, lives in [docs/spec.md](docs/spec.md) §1.2 (non-goals),
§3 (capabilities not provided) and §5 (recorded decisions). The two that come up most:

- **Responses is the only canonical format.** Internal item types are Responses
  types. Another backend is supported by translating inside its adapter
  (`models/anthropic`, spec §5.10) — never by a second canonical format or a
  neutral abstraction layer. Chat Completions is intentionally NOT supported.
- **No hosted tools.** A tool is a `*Tool` **struct**, not an interface,
  so there is nothing a hosted tool could implement. Provider-hosted tools
  (`web_search`, `file_search`, …) are deliberately not modeled — do not
  reintroduce them, and do not reintroduce a `Tool` interface to make room.

## Conventions

- **Behavior is specified, not inherited.** Answer behavior questions from
  `docs/spec.md`. When it does not cover a case: decide, implement, and add the
  invariant to `spec.md` **in the same change**. Open questions live in
  `spec.md` §6 — implementing one means moving it out of §6 first.
- **Upstream watch, not upstream parity.** After each upstream minor release,
  review its changelog and record the decision (ported / declined + why) in
  `docs/upstream_watch.md`. There is no obligation to match.
- **A new black-box test goes in `package agents_test`.** That external test
  package can import `agentstest` (the shipped harness) without a cycle, and
  both are established practice: `agents/` has ~10 `agents_test` files doing
  exactly this. The bulk of the older test files are internal
  (`package agents`) with their own unexported `fakeModel`; they stay where
  they are.
- **Docs track the code.** Any functional change must update the relevant
  `docs/` page — and `README.md` when it affects the feature set or
  quick-start. New public capabilities get a runnable example under
  `examples/`. `cmd/verifydocs` checks that doc snippets still name things that
  exist; it runs in CI, so a rename that leaves the prose behind fails there.

## Principles

- **Simplicity is the default; earn every abstraction.** An interface, wrapper,
  option, or exported symbol needs a *present* caller or a documented external
  contract — no "just in case." A zero-consumer feature is removed, not kept
  (the retired `Logging` middleware is the standing precedent).
- **Reach for the plainer construct first.** A concrete type over an interface, a
  field over a getter, a function over a one-use struct, sequential code over a
  goroutine. Add the seam when the *second* implementation arrives — an interface
  earns its keep only when something else implements it (`Model`, `Storage`,
  `Compactor` all clear this bar; a one-impl interface usually does not).
- **The recorded decisions are deliberate.** Before "simplifying" anything in
  [spec.md §1.2/§3/§5](docs/spec.md), assume it was decided on purpose and read
  the reason. Independent evolution is fine; silent reversal of a recorded
  decision is not.

## Comments

Comment bloat is a recurring regression here — the altitude rule is strict.

- **A comment says WHAT the code does and the one gotcha a reader can't infer.**
  One or two lines. That is the ceiling for internal functions and struct fields.
- **Rationale lives in the spec, not the code.** "Why it's built this way,"
  trade-offs, and invariant proofs go in [spec.md](docs/spec.md). When a subtle
  invariant needs a signpost, write a single line — `// … — see spec §2.7f` —
  never a restatement of the spec paragraph. The spec is the one authority; a
  copy in a comment is a second one that drifts.
- **No historical narrative.** "used to…", "the old API…", "this was a bug
  because…" is git/PR history — delete it. The reader sees only the code that
  exists now.
- **No essays in doc comments.** No markdown headers, no multi-section treatises
  on a type or func. Public godoc may run longer *only* for a runnable example or
  a load-bearing caveat the caller needs.
- **Rough target: ~10–15% comment lines.** A file past ~25% is a smell to review,
  not a hard limit.

## Commits

- **`type(scope): summary`** — `fix` / `refactor` / `feat` / `docs` / `test` /
  `chore`. A breaking API change adds `!`: `refactor(session)!:`.
- **Behavior change ⇒ same-commit spec update.** The invariant in `docs/spec.md`
  and the relevant `docs/` page land in the *same* commit as the code, never a
  follow-up. (Restated from Conventions because it is the rule most often missed.)
- **Branch off `main`; keep CI green.** `./scripts/ci.sh` (race detector on) must
  pass before each commit. Don't push or merge without being asked.
