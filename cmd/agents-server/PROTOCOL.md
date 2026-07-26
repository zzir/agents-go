# Wire protocol — target shape

> This is the shape the WebSocket protocol is being moved *to*. Most of it
> ships today; the exceptions are marked 🚧 below.
>
> It was agreed up front rather than discovered along the way: the frontend and
> the backend change together, and without a stated target they renegotiate the
> payload shape three or four times. Changing a decision here is allowed — but
> it is a decision, made once, not a drift.
>
> The live protocol is defined by
> [`internal/protocol/messages.go`](internal/protocol/messages.go), mirrored in
> `web/src/lib/protocol.ts`. Neither may diverge from the other at any point.

---

## What does not change

- The envelope stays `{"type": "...", "payload": {...}}`.
- Application-level auth stays: `{"type":"auth","token":"..."}` first, server
  replies `auth.ok`. No token in the query string.
- Run events stay a **broadcast bus**: every authenticated connection is
  attached to every run. A dropped socket does not cancel a run, and
  `run.subscribe` with `from_seq` resumes from a cursor.
- Event-type constants stay single-sourced in Go and mirrored in TypeScript.
  A typo must remain a compile error, never a silently-undelivered event.

---

## F1 · Every streaming delta carries an entry id

**The single most important change.** Today `run.step` is `{run_id, delta}` —
there is no way to tell which item a delta belongs to. `run.message` carries an
`item_id`, but the deltas that preceded it do not. The frontend therefore
attributes deltas by *position*, and `streamReducer.mergeLiveTail` exists to
reconcile the guess against what actually got persisted.

That reconciliation is the root cause of the streaming state-machine bugs
fixed one by one in earlier work. It goes away when deltas are attributable.

**Frozen:**

```jsonc
// run.delta — replaces run.step and run.reasoning
{
  "run_id":   "...",
  "entry_id": "msg_abc",     // REQUIRED. Ties the delta to its entry.
  "field":    "text",        // "text" | "reasoning"
  "delta":    "partial cont"
}
```

**Client rule, frozen:** a delta appends into a *provisional* buffer keyed by
`entry_id`. The completed entry (F2) **replaces** that buffer wholesale — it is
never merged into it. An entry that arrives with no preceding delta renders
immediately; deltas that arrive for an entry already completed are discarded.

This keeps bandwidth O(n) — the alternative of resending the whole entry per
token is O(n²) on long answers — while removing every case where the live view
and the persisted view can disagree.

---

## F2 · One `run.entry` event replaces six item events

Today six events carry items, each with its own payload shape:
`run.message`, `run.reasoning_item`, `run.tool_call`, `run.tool_result`,
`run.handoff`, `run.compaction`. Each addition means a new event type, a new
reducer branch, and a new persisted-vs-live shape to reconcile.

The SDK's `SessionEntry` makes them one thing.

**Frozen:**

```jsonc
// run.entry — one completed entry, authoritative
{
  "run_id": "...",
  "entry": {
    "id":       "msg_abc",
    "seq":      41,               // storage-assigned, monotonic per session
    "kind":     "item",           // item | annotation | compaction | terminal | custom
    "parent_id": "msg_aba",       // tree model; empty at a root
    "source":   { "type": "model" },   // model | user | tool | handoff | error_handler | compaction
    "display":  { /* F4 */ },
    "usage":    { "input_tokens": 0, "output_tokens": 0 },
    "payload":  { /* kind-specific; for kind=item this is the Responses item verbatim */ }
  }
}
```

**Frozen invariant:** the entry delivered over the wire is **byte-identical in
shape** to the entry returned by the REST history endpoint. One shape, one
renderer, no adapter. This is what makes `mergeLiveTail` deletable rather than
merely smaller.

`kind` is an open vocabulary — an unknown `kind` renders through the `custom`
fallback rather than being dropped. Clients must not switch exhaustively on it.

**Not replaced by `run.entry`** (they are run lifecycle, not content):
`run.started`, `run.agent_start`, `run.output`, `run.error`, `run.interrupted`,
`run.cancelled`.

`run.compaction` survives as a *progress* signal (`phase: started|finished`)
while the compaction checkpoint itself arrives as a `run.entry` with
`kind: "compaction"`. Progress is transient; the checkpoint is history.

---

## F3 · `run.error.code` mirrors the SDK `ErrorCode`

Today six codes are hand-written in `protocol` and hand-mapped in
`bridge/runner.go`. The SDK now owns the vocabulary and the bridge
calls `agents.CodeOf(err)`.

**Frozen split:**

| Origin | Examples | Owner |
|---|---|---|
| SDK error codes | `guardrail_tripwire`, `max_turns_exceeded`, `model_behavior`, `tool_timeout`, `user_error` | `agents.ErrorCode`, mirrored into `protocol.ts` |
| Transport-only codes | `session_busy`, `session_not_found`, `run_not_found`, `approval_failed`, `config_error` | agents-server; no SDK equivalent exists or should |

**Frozen:** the two sets share one flat namespace on the wire and must not
collide. A client that does not recognize a code falls back to generic error
rendering — the set grows without a client release.

The guardrail extras (`guardrail`, `stage`) generalize: `stage` now takes the
four `GuardrailStage` values (`input` / `output` / `tool_input` /
`tool_output`), not just two.

---

## F4 · `display` is a structured projection, not a string

Today `run.tool_result` is `{output: string}` and the frontend parses text to
decide how to render. The SDK's `ItemDisplay` + `ToolResult.Display`
replace that with a projection the producer chooses.

**Frozen:**

```jsonc
"display": {
  "renderer": "diff",        // hint only; unknown renderer → "text" fallback
  "title":    "edit_file",
  "summary":  "3 files changed",
  "detail":   "...",          // optional long form, collapsed by default
  "extra":    { }             // renderer-specific, opaque to the generic path
}
```

**Frozen invariant:** `display` is a *rendering hint*. A client that ignores it
entirely must still produce a correct, readable timeline from `payload` alone.
This keeps `display` free to evolve without a lockstep frontend release.

Streaming partial tool results (`ToolContext.Emit`) reuse F1: a
`run.delta` with `field: "display.detail"` and the tool call's `entry_id`.

---

## F4a · `run.gap` — a connection that fell behind is told

**Landed.** A connection whose buffer overflows receives, on that connection
only, `{run_id, dropped, last_good, next}`. The run is unaffected; its other
subscribers are unaffected.

The client refetches (or resubscribes with `from_seq = last_good`). Before this,
the SSE handler dropped silently on a full buffer and the client had no way to
know — the timeline was quietly incomplete.

---

## F5 · Uplink gains the three queue semantics

`RunControl` has three ways to inject into a live run. They are
distinct semantics, not one endpoint with a mode flag.

**Frozen:**

| type | Meaning |
|---|---|
| `run.steer` | Inject into the **current** turn — the model sees it before its next step |
| `run.follow_up` | Queue for **after** the current run ends; starts a new run automatically |
| `run.next_turn` | Inject at the **next turn boundary** of the current run |

All three: `{run_id, input}`. `run.cancel` keeps its existing
`mode: ""|"abort"|"graceful"`.

**Implemented** (step 24). The SDK side is `RunControl.Steer` / `NextTurn` /
`FollowUp`; the hub routes by envelope type rather than a mode field, because
these are three intentions and the client says which one it means.

Delivery is confirmed by silence: a run that is no longer accepting input
answers `run.error` with `run_not_found`. Input the run never consumed is
reported by `RunControl.Pending()` rather than dropped — the user typed
something and it must not vanish.

---

## F6 · Runs report a phase

**Frozen:** `run.phase` — `{run_id, phase}` where phase is one of
`model` | `tools` | `guardrails` | `compaction` | `waiting_approval`.

The UI shows what a run is *doing* during a long silence. Purely advisory;
a client may ignore it. It is also a tracing span attribute.

---

## F7 · History reads are cursor-paginated

**Frozen:** history is fetched by cursor, not offset —
`GET /api/v1/sessions/{id}/entries?after=<seq>&limit=N`, returning
`{entries: [...], next_cursor: "<seq>"}`.

`seq` is storage-assigned and monotonic per session, so a cursor
stays valid across concurrent appends. Offset pagination cannot promise that.

---

## F8 · Sessions are trees

**Frozen:** an entry carries `parent_id`; a session carries a `leaf_id`.
Switching branches is `PATCH /api/v1/sessions/{id} {leaf_id}` — a **persistent**
operation, not a client-side view state.

`GET .../entries` returns the path from the current leaf to the root, in order.
Forking returns a new session id whose entries share ancestry.

---

## Status

| | Shipped |
|---|---|
| F3 error codes | ✅ |
| F4 structured display | ✅ |
| F4a `run.gap` | ✅ |
| F5 uplink queues | ✅ |
| F7 cursor pagination | ✅ |
| F8 session trees | ✅ |
| F1 delta entry ids | 🚧 |
| F2 `run.entry` | 🚧 |
| F6 phase | 🚧 |

F1, F2 and F6 travel together: they replace per-delta text with per-entry
snapshots, which is a protocol change whose payoff is on the client — roughly
half of the streaming reducer's transforms become a replace-by-id. The half
that would NOT go away is the reconciliation between a REST history fetch and
the live events that arrived while it was in flight, which is a client-side
ordering problem the payload shape does not touch. Weigh that before starting.

No row may land in the Go protocol without the matching `protocol.ts` change in
the same commit.

---

## Discipline

1. **`messages.go` is the source of truth**; `protocol.ts` mirrors it. Same
   commit, always.
2. **No string literals** for event types or error codes on either side.
3. **Unknown values degrade, never drop** — unknown `kind`, `renderer` and
   `code` all have defined fallbacks above. This is what lets the server ship
   ahead of the client.
4. **Changing this document is a decision**, recorded here with its reason —
   not an edit made in passing while implementing something else.
