# Architecture

How the pieces compose, and where you can get in between them.

The other pages explain each capability on its own. This one is about the
shape: what a run passes through, which types are the seams, and why the
boundaries fall where they do. [spec.md](../reference/spec.md) is the authority on
behavior — where the two disagree, spec.md is right.

---

## The core type is a struct

`agents.Agent` is a plain struct, not an interface. It holds instructions,
tools, handoffs, guardrails and model settings; it has no `Run` method.
Everything happens in the **runner**, which takes an agent as data.

That is what makes an agent cheap to build per request, safe to copy and
modify, and trivial to serialize into a config. It also means there is exactly
one run loop to reason about, rather than one per agent implementation.

```go
res, err := agents.RunSync(ctx, agent, "hello", agents.RunOptions{
    Model: agents.ModelOptions{Provider: provider},
})
```

---

## What a run passes through

```
Run(ctx, agent, input, opts)
  │
  ├─ input normalization ──────────── once, up front, so every middleware
  │                                    inspects the same item list
  ├─ middleware chain ─────────────── Loop, Approval, Retry, yours
  │
  └─ run loop ─── per turn ──────────┐
       ├─ budget / cancellation check │
       ├─ resolve model, instructions, tools, handoffs, output schema
       ├─ build model input ←──────── session projection (see below)
       ├─ input guardrails (first turn, concurrent with the model call)
       ├─ CALL THE MODEL ←─────────── retry / fallback / routing decorators
       ├─ classify output ─────────── message · tool call · handoff ·
       │                              reasoning · unknown (kept verbatim)
       ├─ execute side effects ────── tools concurrently, approval gate first
       ├─ persist ─────────────────── append entries for the turn
       └─ decide ──────────────────── continue · final output · interrupt
```

Two properties of this loop are worth knowing before you build on it:

**A run executes on the consumer's goroutine.** `Run` returns an
`iter.Seq2`; ranging it advances the loop. Abandoning the range stops the run
where it stands — there is no producer goroutine to leak, and no context you
must remember to cancel. The trace still closes, because the loop unwinds
through its own defers.

**Streaming and blocking are one loop.** `Run` streams the model call so raw
events reach you; `RunSync` makes one blocking call. Everything else —
guardrail timing, persistence points, ordering, tracing — is identical code.
A change to run semantics is written once.

See [Running agents](../howto/running_agents.md) and [Streaming](../howto/streaming.md).

---

## Sessions are three layers

This is the part most worth understanding, because it is where the SDK differs
most from a naive "list of messages".

| Layer | Type | Knows about |
|---|---|---|
| Storage | `session.Storage` | Reading and writing entries. Nothing about meaning. |
| Semantics | `session.Session` (a struct, not an interface) | Turning entries into what the model reads. |
| Projection | `session.Projector` | Which entry kinds reach the model at all. |

A session stores **entries**, not bare items. An entry carries the item plus
everything about it worth keeping: who produced it (`Source`), what it looks
like to a person (`Display`), what it cost (`Usage`), what went wrong along the
way (`Diagnostics`), and its place in the tree (`ParentID`).

Three consequences fall out of that:

- **Append-only.** Nothing is rewritten. A display that settles after its turn
  ended — a background task finishing, a late diagnostic — is a *new update
  entry* naming its target, folded in at read time. That is what removes the
  "the task finished before the turn was saved" race rather than handling it.
- **Branching is a fold, not a pointer.** Switching to another attempt appends
  a leaf entry; the active branch is derived by walking parent links. The
  abandoned attempt stays recorded, which is what makes "show me the other
  answer" possible at all.
- **Compaction appends a checkpoint.** The folded entries stay; the checkpoint
  names them and carries the summary; the projection drops what was folded and
  renders the summary up front, so the model sees `[summary, kept…]` and a
  reader can still expand what was folded.

See [Sessions](../howto/sessions.md) and [Context management](../howto/context.md).

---

## Extension points

Everything below is an interface you can implement. Nothing else in the SDK is
meant to be replaced.

| Seam | Interface | Use it to |
|---|---|---|
| Model backend | `Model`, `ModelProvider` | Talk to something other than the OpenAI Responses API |
| Model behavior | decorators: `NewRetryModel`, `NewFallbackModel`, `RouterProvider`, `NewStreamOnlyModel` | Add retry, failover, per-prefix routing or stream-only adaptation without touching the loop |
| Tools | `NewTool[Args, Result]` | Give the agent something to do. A tool is a `*Tool` **struct**, not an interface — every tool executes locally |
| Tool behavior | fields on `Tool` | Add approval, a timeout, guardrails, sequencing — set the field, or copy the struct for a tool you did not build |
| Run behavior | `RunMiddleware` | Wrap a whole run: logging, approval policy, retry-the-run |
| Storage | `session.Storage` (+ optional `session.AtomicReplacer`, `session.GuardedReplacer`) | Persist entries anywhere |
| Context shaping | `Compactor` (+ optional `CompactionCheckpointer`) | Decide what history the model sees when it gets long |
| Safety | `Guardrail` | Inspect input, output, tool arguments or tool results — one value can cover several stages |
| Observability | `Tracer`, `Processor` | Send spans somewhere |
| Execution | `Sandbox` | Run model-generated commands under isolation |

**The tool seam is a struct on purpose.** A capability is a field, so a
variant of someone else's tool is `cp := *tool` and nothing can hide a
capability behind a wrapper — [spec.md §2.7c](../reference/spec.md#27c-tool-capabilities-are-fields)
states the rule, [decisions §5.4](decisions.md#54-a-tool-is-a-struct-not-an-interface)
the reasoning.

---

## Packages

Core module path: `github.com/zzir/agents-go`. This table names the import
paths; which of them are separate **modules**, and why, is the next section.
Signatures live on
[pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go).

| Package | What it is |
|---|---|
| `agents` | Core: agents, runner, tools, guardrails, sessions, HITL, tracing hooks |
| `models/openai` | OpenAI Responses API model provider (built on `openai-go` v3) |
| `models/modelkit` | Dependency-free toolkit for model adapters + `conformancetest` golden matrix |
| `tracing` | Traces, spans, processors and exporters |
| `sandbox` | `Sandbox` interface + `CodeTool` + `apply_patch` + the local backend (three backends in all: local, `sandbox/docker`, `sandbox/e2b`; each hosts persistent shells and terminals) |
| `sandbox/e2b` | E2B-compatible cloud backend (HTTP only, so it stays in the root module) |
| `sandbox/sandboxtest` | conformance suite every `Sandbox` backend runs against |
| `mcp` | **separate module** — Model Context Protocol client (modelcontextprotocol/go-sdk) |
| `models/anthropic` | **separate module** — Anthropic Messages API backend (translated to Responses) |
| `sandbox/docker` | **separate module** — Docker sandbox backend |
| `sessions` | **separate module** — SQLite/PostgreSQL session store (uptrace/bun) |
| `skills` | **separate module** — Agent Skills (`SKILL.md`) parser |
| `cmd/agents-server` | **separate module** — the workbench: web app (REST + WS + embedded UI) over the SDK |

The core lives in a single `agents/` package. The original plan split it further
into `tools/`, `outputs/` and `models/`, but in Go those would form an import
cycle with the core (tool callbacks reference `RunContext`; the `Model` interface
references `Tool`), so they are kept together in `agents/`. Provider, storage and
tracing implementations live in subpackages that import `agents`; MCP does the
same from a nested module, so its go-sdk dependency stays out of the core. Items
use the `openai-go` Responses types as the wire format directly, so nothing is
lost converting in and out of a parallel item model.

The Responses **WebSocket transport** and a `Model` connection-lifecycle hook
(`Close`/`aclose`) are not implemented — only the HTTP Responses transport is
supported today.

---

## Module boundaries

The repository is a Go workspace of eight modules (one of them an example
module with its own heavy dep). **A submodule exists only to keep a heavy
dependency out of the core.** Anything dependency-free stays in the root
module, no matter how peripheral it feels — `models/modelkit` is the standing
example: shared adapter plumbing, stdlib-only, so it lives in root.

| Module | Exists because of |
|---|---|
| root | — the SDK (including `models/openai` and `models/modelkit`) |
| `mcp` | the modelcontextprotocol/go-sdk client and the seven indirect requirements it brought with it |
| `models/anthropic` | the anthropic-sdk-go client |
| `sandbox/docker` | the Docker client (and x/crypto/ssh for remote daemons) |
| `sessions` | the SQL drivers |
| `skills` | the YAML parser |
| `cmd/agents-server` | a web application, not a library |
| `examples/anthropic` | an example program needing that same heavy dep |

CI builds each module standalone with `GOWORK=off`, so a workspace-only fix
cannot hide a missing `go.mod` require.

This is also why tracing is vendor-neutral in the core: a span is a `Type` tag
plus a `Data map[string]any`, and mapping it onto a vendor's model is the
consumer's `Processor` to write ([decisions §5.6b](decisions.md#56b-tracing-stays-vendor-neutral-otel-export-is-the-consumers-job)).
Adding an OTel dependency to the core would tax every user for a capability
most do not enable.

---

## Where behavior is decided

- **[spec.md](../reference/spec.md)** — the invariants: what is always true.
  When something is not covered there, the rule is: decide, implement, and add
  the invariant in the same change.
- **[decisions.md](decisions.md)** — why each one went the way it did. Read the
  reason before reopening a decision.
- **[scope.md](scope.md)** — what the project deliberately does not do.
- **[upstream_watch.md](upstream_watch.md)** — what has been reviewed from the
  Python SDK, and what was declined. There is no obligation to match it.

---

## The workbench's architecture

Everything above is the SDK. `agents-server` — the workbench that runs on top of
it — composes as follows. The rules its handlers and panels must obey are in
[workbench design invariants](workbench-invariants.md).

```
cmd/agents-server/
├── main.go                     entry point
├── cmd/root.go                 CLI flags, start-up and shutdown ordering
├── cmd/wire.go                 the composition root: stores → bridge → handlers → auth → server
├── internal/
│   ├── server/                 Gin engine, routing, WS upgrade + heartbeat
│   │   ├── auth.go             bearer middleware (AuthFunc), the auth-exempt list
│   │   ├── ratelimit.go        per-IP budgets; AuthGuard (failed-credential budget)
│   │   ├── audit.go            the audit middleware (successful mutating requests)
│   │   ├── server.go           engine setup, body cap, CSP, static SPA
│   │   └── ws.go               WS upgrade + heartbeat
│   ├── authn/                  who is calling: token mode, OAuth (PKCE) login, PATs
│   ├── secrets/                AES-256-GCM box that seals stored credentials
│   ├── handler/                HTTP handlers (one file per resource)
│   │   ├── authz.go            the authorization rules as route gates (decisions §5.29)
│   │   └── conn_registry.go    per-owner WebSocket broadcast bus
│   ├── bridge/                 the runner and what it orchestrates
│   │   ├── agent.go            assemble a full agent from DB config
│   │   ├── runner.go           stream execution, resume after approval
│   │   ├── stream_bridge.go    SDK stream events → protocol envelopes
│   │   ├── run_hub.go          per-run event hub (buffering, seq resume, status)
│   │   ├── approvals.go        HITL approval persistence & resolution
│   │   ├── retention.go        the maintenance loops: approval reaper, trace/audit/token/wake-up pruning
│   │   └── ...                 tasks, workflows, triggers, tracing, provider resolve
│   ├── mcpservers/             live MCP connections behind stored configs; the MCP OAuth flow
│   ├── providers/              the registry of model-provider backends; the ChatGPT login
│   ├── sandboxes/              live sandbox instances behind stored configs; exec_command trust
│   ├── guardrails/             stored + built-in guardrail definitions → SDK guardrails
│   ├── settings/               the settings registry and the typed reader (incl. the proxy client)
│   ├── logging/                structured logging + context propagation
│   ├── docs/                   generated OpenAPI 3.1 document, swagger.yaml (make openapi)
│   ├── store/                  data layer (bun ORM; SQLite or PostgreSQL, 24 tables — see Database)
│   ├── protocol/               wire types — WS messages, REST error envelope, the audit record
│   └── web/                    embedded SPA static files
```

### Request flow

1. A client starts a run — `run.create` over WebSocket or `POST /sessions/:id/runs`
   over REST. Both call `runner.StartRun`, which registers the run in the shared
   run hub and executes it in the background, independent of the caller's
   connection.
2. The runner loads config from the database and calls `BuildFullAgent` to
   assemble the agent with its provider, MCP tools, sandbox, guardrails,
   memories, and hooks, then calls the SDK's `agents.Run()` to execute.
3. Streaming events are published to the hub, which fans them out to every
   subscriber (WebSocket connections and SSE streams) and buffers them for replay
   so a reconnecting client can resume from a sequence number.
4. If a tool requires approval, the run pauses and the pending approval is
   persisted; it resumes on `approve`/`reject` (over either transport) and
   survives a server restart.
5. History is persisted per turn as the run progresses — a cancelled or failed
   run keeps every completed turn — and the session title is generated in
   parallel with the first run rather than after it.
