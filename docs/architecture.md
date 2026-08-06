# Architecture

How the pieces compose, and where you can get in between them.

The other pages explain each capability on its own. This one is about the
shape: what a run passes through, which types are the seams, and why the
boundaries fall where they do. [spec.md](spec.md) is the authority on
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
  ├─ middleware chain ─────────────── Loop, Approval, Retry, Logging, yours
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

See [Running agents](running_agents.md) and [Streaming](streaming.md).

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

See [Sessions](sessions.md) and [Context management](context.md).

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
| Storage | `session.Storage` (+ optional `session.AtomicReplacer`, `session.GuardedReplacer`, `session.EntryPopper`) | Persist entries anywhere |
| Context shaping | `Compactor` (+ optional `CompactionCheckpointer`) | Decide what history the model sees when it gets long |
| Safety | `Guardrail` | Inspect input, output, tool arguments or tool results — one value can cover several stages |
| Observability | `Tracer`, `Processor` | Send spans somewhere |
| Execution | `Sandbox` | Run model-generated commands under isolation |

**The tool seam is a struct on purpose.** Tools used to be a sealed interface
with eight optional side interfaces, reached through a `ToolAs[T]` walker,
because behavior was added by wrapping. Every one of those wrappers only set
what was already a field on the single concrete type, and the walker existed
because a plain type assertion through a wrapper silently returned false — a
timeout wrapper around an approval wrapper reported that the tool needed no
approval. Fields have no such failure mode, and a variant of someone else's tool
is `cp := *tool`. See [spec.md §2.7c](spec.md#27c-tool-capabilities-are-fields-).

---

## Module boundaries

The repository is a Go workspace of twelve modules (three of them example
modules with their own heavy deps). **A submodule exists only to keep a heavy
dependency out of the core.** Anything dependency-free stays in the root
module, no matter how peripheral it feels — `models/modelkit` is the standing
example: shared adapter plumbing, stdlib-only, so it lives in root.

| Module | Exists because of |
|---|---|
| root | — the SDK (including `models/openai` and `models/modelkit`) |
| `mcp` | the modelcontextprotocol/go-sdk client and the seven indirect requirements it brought with it |
| `models/anthropic` | the anthropic-sdk-go client |
| `sandbox/docker`, `sandbox/ssh` | the Docker and SSH client libraries |
| `sessions` | the SQL drivers |
| `skills` | the YAML parser |
| `tracing/otel` | the OpenTelemetry SDK |
| `cmd/agents-server` | a web application, not a library |
| `examples/otel`, `examples/anthropic`, `examples/mcpserver` | example programs needing those same heavy deps |

CI builds each module standalone with `GOWORK=off`, so a workspace-only fix
cannot hide a missing `go.mod` require.

This is also why tracing is vendor-neutral in the core: a span is a `Type` tag
plus a `Data map[string]any`, and the OpenTelemetry mapping lives in a separate
module. Adding the OTel dependency to the core would tax every user for a
capability most do not enable.

---

## Where behavior is decided

- **[spec.md](spec.md)** — the invariants, each with the reason it is what it
  is. When something is not covered there, the rule is: decide, implement, and
  add the invariant in the same change.
- **[upstream_watch.md](upstream_watch.md)** — what has been reviewed from the
  Python SDK, and what was declined. There is no obligation to match it.
