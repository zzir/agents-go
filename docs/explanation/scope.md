# Scope

What this project is, and what it deliberately is not. The reasoning behind a
given line usually continues in [design decisions](decisions.md); the behavior
that results is in [the spec](../reference/spec.md).

---

## 1. Scope

### 1.1 What this is

The agent workbench you run yourself: see what the model saw, replay it,
fork it — **"Go agents. Local first."** Concretely, `agents-server`: one
binary, your data in SQLite (or PostgreSQL), an embedded UI, and a debug loop
in which the transcript is the truth, a context lens and traces show what the
model was sent, and any turn can be replayed or forked.

The SDK underneath it — the root module of this repository — is the same core
consumed a second way: embedded in your own Go program, with no dependency on
or reporting to the workbench ([§1.2](#12-non-goals), last row). Two consumers,
one core, one dependency edge.

**The first conversation is frozen.** It is one process, SQLite by default,
and no Docker: a downloaded binary, one API key and a browser reach a
streamed reply with the Inspector open. Sandboxes, PostgreSQL, deployment,
team sign-in and anything else that needs a second thing installed are
power-user steps that come *after* it. The getting-started pages — the
[README](../../README.md) and
[Running the workbench](../tutorial/workbench.md) — keep them out of that
path, and a change that puts them back is a scope change, not an edit.

---

### 1.2 Non-goals

| Not doing | Why |
|---|---|
| **Chat Completions API** | Internal item types *are* Responses types ([§5.5](decisions.md#55-internal-item-types-are-responses-wire-types)). A backend that speaks another protocol is supported by translating at the model boundary ([§5.10](decisions.md#510-non-responses-backends-adapt-at-the-model-boundary)) — never by making a second format canonical. Chat Completions specifically was declined again 2026-07-31 in favor of a native Anthropic adapter; revisit only with a concrete backend nothing else covers. |
| **Provider-hosted tools** (`web_search`, `file_search`, `code_interpreter`, `computer_use`, …) | A tool is a `*Tool` struct, not an interface, so there is nothing a hosted tool could implement; every tool executes locally. Hosted tools bind a tool to one backend. |
| **A neutral multi-provider abstraction** | No lowest-common-denominator message model. An adapter implements `Model` by translating to the canonical Responses format ([§5.10](decisions.md#510-non-responses-backends-adapt-at-the-model-boundary)); `models/modelkit` is shared plumbing for writing adapters, not an abstraction layer. The SDK guarantees depth of correctness for Responses semantics. |
| **Model price or capability tables** | They change constantly and do not belong in an SDK. `Usage` exposes raw token counts; pricing is the caller's concern. |
| **Realtime and voice** | A different interaction model, out of scope. |
| **Graph orchestration as the multi-agent primitive** | Handoffs already cover "switch agent at runtime". Graph orchestration, if ever needed, layers *on top* — see [§5.1](decisions.md#51-handoffs-stay-graph-orchestration-does-not-replace-them). |
| **The SDK reporting to the workbench** | `cmd/agents-server` (the workbench) depends on the SDK; the SDK knows nothing of the workbench. A program written against the SDK runs on its own, with the SDK's session stores and tracing `Processor`s — no trace-ingest endpoint, no remote `session.Storage` pointing at the server, no "register my program" bridge. The workbench runs the agents configured in it; the SDK runs agents in your program. Two consumers of one core, one dependency edge. Decided 2026-08-24 with the positioning "Go agents. Local first."; revisit only if the workbench's own debug loop (traces, replay, fork) turns out to need data a configured agent cannot produce. |

---

## 3. Capabilities deliberately not provided

Beyond the non-goals in [§1.2](#12-non-goals):

| Not provided | Why |
|---|---|
| A built-in default model | The SDK does not guess which model you want. With none configured, `Model` returns a `*UserError`. |
| Implicit model-parameter injection (e.g. reasoning defaults for a model family) | Explicit beats implicit. Set `ModelSettings` yourself. |
| A free-form request passthrough dict | `ExtraBody` / `ExtraHeaders` / `ExtraQuery` cover it, and they are typed. |
| Redis / encrypted session backends | Implement `session.Storage`. The SDK ships in-memory and SQL (SQLite/PostgreSQL). |
| A pop/undo storage primitive | A run never pops (entries are append-only, spec §2.5b), and a host that wants "undo" has its own deletion primitive against its own store. |
| The Responses WebSocket transport, and a `Model` connection-lifecycle hook (`Close`) | Only the HTTP Responses transport is implemented; a `Model` has no lifecycle the runner manages. |
| A REPL and graph visualization | Not an SDK concern. |
| A graph / fan-out orchestrator on top of tasks (map over N inputs, join, branch on model choice) | A task's work may span several runs (`Config.Continue`, §2.13): a fixed sequence, a loop until a check passes — one job, one session, one transcript, which is what keeps it cheap and legible. Fanning out into N parallel children with a join is a different thing: N sessions, N transcripts, a merge nobody has designed the semantics of yet, and a step toward the general workflow engine handoffs and tasks were chosen over (§5.1). Parallel work is what `spawn_task` is for; a host that needs a join writes it against the task API. |

---

## Roadmap

Named, not promised — what is being considered next for the workbench. Anything
here that turns into a rule graduates into
[the spec](../reference/spec.md) or
[workbench design invariants](workbench-invariants.md); anything decided
against graduates into §1.2 above.

- **Multiple instances.** Two processes on one PostgreSQL do not cooperate
  yet — not even a rolling restart: the truth about a live run is in process
  memory. What is process-local today, and how it breaks with a second
  instance: `RunHub` (a run is 404 everywhere but its instance; the
  one-run-per-session rule holds only by the entries unique index); the
  orphan sweep at startup fails every `working` task, including the other
  instance's; the `Waker` reads the local hub, so two instances can wake one
  session into concurrent runs; `ConnRegistry` broadcasts reach local
  connections only; the cron table is loaded per instance, so every schedule
  fires once per instance; the webhook replay guard, the OAuth pending-login
  and exchange maps, the MCP OAuth callback channel, refresh-token dedup, the
  sandbox instance cache and terminal fences, exec_command trust, and the
  rate budgets are all in-memory maps. Already shared through the database:
  pending approvals, wake-up debts, auth tokens, the audit log, ownership.
  The direction chosen: shard by user (sticky load-balancing on the user
  id), an `instance_id` with a heartbeat table and lease-based ownership
  instead of "is it in my memory", maintenance loops kept idempotent,
  SQLite refused for more than one instance — and, before any of it ships, a
  migration mechanism,
  because a rolling upgrade is two binaries on one schema. Until then a startup
  advisory lock on PostgreSQL refuses a second instance outright, so the sweep
  above cannot fire against a live instance's tasks (invariant 63).
- **Guardrail ordering at the approval gate.** The tool stages are configurable
  now (a guardrail's `stages` cover `tool_input` / `tool_output` for every tool
  call), but `RunOptions.Exec.PreApprovalToolInputGuardrails` is not exposed
  as an agent config field. With it on, a guardrail rejection resolves an
  approval-gated call without a human round-trip. Per-TOOL binding — "only this
  tool's arguments go through this guardrail" — is a separate thing the SDK
  does not model; it would need a `Stages`-like selector keyed by tool name.
- **Renderer hints on tool-call cards.** The `display.renderer` hint
  ("terminal", "diff", "table") travels end to end — `ToolResult.Display` to
  the stored display JSON to the timeline, live and replay — but
  `ToolCallCard` does not branch on it yet: a terminal view for shell output
  and a diff view for a patch are the remaining work.
