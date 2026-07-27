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
| Storage | `SessionStorage` | Reading and writing entries. Nothing about meaning. |
| Semantics | `Session` (a struct, not an interface) | Turning entries into what the model reads. |
| Projection | `EntryProjector` | Which entry kinds reach the model at all. |

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
| Model behavior | decorators: `NewRetryModel`, `NewFallbackModel`, `RouterProvider` | Add retry, failover or per-prefix routing without touching the loop |
| Tools | `NewFunctionTool[Args, Result]` | Give the agent something to do. The `Tool` interface is **sealed** — every tool executes locally |
| Tool capabilities | side interfaces, reached with `ToolAs[T]` | Add behavior (approval, timeout, sequential execution) that survives decorator stacking |
| Run behavior | `RunMiddleware` | Wrap a whole run: logging, approval policy, retry-the-run |
| Storage | `SessionStorage` (+ optional `AtomicReplacer`, `EntryPopper`) | Persist entries anywhere |
| Context shaping | `Compactor` (+ optional `CompactionCheckpointer`) | Decide what history the model sees when it gets long |
| Safety | `Guardrail` | Inspect input, output, tool arguments or tool results — one value can cover several stages |
| Observability | `Tracer`, `Processor` | Send spans somewhere |
| Execution | `Sandbox` | Run model-generated commands under isolation |

**`ToolAs[T]` deserves a note.** Tool decorators stack, and a plain type
assertion only ever sees the outermost one — a timeout wrapper around an
approval wrapper loses the approval. `ToolAs[T]` walks an `Unwrap()` chain the
way `errors.As` does, so a capability is found wherever it sits in the stack.
This was measured before it was designed: every stacking order lost every
capability except the outermost. See
[spec.md §2.7c](spec.md#27c-tool-capabilities-are-side-interfaces-), which
states the rule that follows from it — a bare type assertion is a bug.

---

## Module boundaries

The repository is a Go workspace of seven modules. **A submodule exists only to
keep a heavy dependency out of the core.** Anything dependency-free stays in the
root module, no matter how peripheral it feels.

| Module | Exists because of |
|---|---|
| root | — the SDK |
| `sandbox/docker`, `sandbox/ssh` | the Docker and SSH client libraries |
| `sessions` | the SQL drivers |
| `skills` | the YAML parser |
| `tracing/otel` | the OpenTelemetry SDK |
| `cmd/agents-server` | a web application, not a library |

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
