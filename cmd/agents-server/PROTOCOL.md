# Wire protocol — target shape

> What the WebSocket protocol is still being moved *toward*, and the decisions
> the code cites by number. Agreed up front rather than discovered along the
> way: the frontend and the backend change together, and without a stated
> target they renegotiate the payload shape three or four times. Changing a
> decision here is allowed — but it is a decision, made once, not a drift.
>
> The protocol that ships today is documented in
> [the wire surface](../../docs/reference/protocol.md#websocket-protocol); the
> live definition is
> [`internal/protocol/messages.go`](internal/protocol/messages.go), mirrored in
> [`internal/web/frontend/src/lib/protocol.ts`](internal/web/frontend/src/lib/protocol.ts).
> Neither may diverge from the other at any point.

---

## What does not change

- The envelope stays `{"type": "...", "payload": {...}}`.
- Application-level auth stays: `{"type":"auth","token":"..."}` first, server
  replies `auth.ok`. No token in the query string.
- Run events stay a **broadcast bus, per owner**: every connection of a
  session owner's is attached to that owner's runs, and nobody else's hears
  them. A dropped socket does not cancel a run, and `run.subscribe` with
  `from_seq` resumes from a cursor.
- Event-type constants stay single-sourced in Go and mirrored in TypeScript.
  A typo must remain a compile error, never a silently-undelivered event.

---

## F1 · Every streaming delta carries an entry id — 🚧 open

**The single most important change.** Today `run.step` is `{run_id, delta}` —
there is no way to tell which item a delta belongs to. `run.message` carries an
`item_id`, but the deltas that preceded it do not. The frontend therefore
attributes deltas by *position*, and `streamReducer.mergeLiveTail` exists to
reconcile the guess against what actually got persisted. It goes away when
deltas are attributable.

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

## F2 · One `run.entry` event replaces six item events — 🚧 open

Today six events carry items, each with its own payload shape:
`run.message`, `run.reasoning_item`, `run.tool_call`, `run.tool_result`,
`run.handoff`, `run.compaction`. Each addition means a new event type, a new
reducer branch, and a new persisted-vs-live shape to reconcile.

The SDK's `session.Entry` makes them one thing.

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

F1 and F2 travel together: they replace per-delta text with per-entry
snapshots, which is a protocol change whose payoff is on the client — roughly
half of the streaming reducer's transforms become a replace-by-id. The half
that would NOT go away is the reconciliation between a REST history fetch and
the live events that arrived while it was in flight, which is a client-side
ordering problem the payload shape does not touch. Weigh that before starting.

---

## F3 · `run.error.code` mirrors the SDK `ErrorCode` — shipped

The SDK owns the error vocabulary and the bridge calls `agents.CodeOf(err)`.

**Frozen split:**

| Origin | Examples | Owner |
|---|---|---|
| SDK error codes | `guardrail_tripwire`, `max_turns_exceeded`, `model_behavior`, `tool_timeout`, `user_error` | `agents.ErrorCode`, mirrored into `protocol.ts` |
| Transport-only codes | `session_busy`, `session_not_found`, `run_not_found`, `approval_failed`, `config_error` | agents-server; no SDK equivalent exists or should |

**Frozen:** the two sets share one flat namespace on the wire and must not
collide. A client that does not recognize a code falls back to generic error
rendering — the set grows without a client release. The guardrail extras
(`guardrail`, `stage`) take the four `GuardrailStage` values.

---

## F4 · `display` is a structured projection, not a string — shipped

`display` is the SDK's `agents.ItemDisplay`, serialized as-is — the field list
follows the SDK, not this document (`kind`, `renderer`, `title`, `summary`,
`text`, `call_id`, `tool_name`, `arguments`, `output`, `is_error`, `extra`).
The producer chooses the projection; the frontend never parses text to decide
how to render.

**Frozen invariant:** `display` is a *rendering hint*. A client that ignores it
entirely must still produce a correct, readable timeline from `payload` alone.
This keeps `display` free to evolve without a lockstep frontend release.

Streaming partial tool results (`ToolContext.Emit`) ship today as their own
event, `run.tool_progress` (`{run_id, call_id, tool_name, delta, renderer?}`);
under F1 they become a `run.delta` with `field: "display.detail"` and the tool
call's `entry_id`.

---

## Shipped elsewhere

Documented where they live now, in [the wire surface](../../docs/reference/protocol.md):

- **F4a `run.gap`** — a connection that fell behind is told, on that
  connection only (`{run_id, dropped, last_good, next}`).
- **F5 uplink queues** — `run.inject` `{run_id, queue: steer|next_turn|follow_up, input}`;
  `run.cancel` keeps `mode`.
- **F6 run phase** — dropped 2026-08-04: the stream's own events carry more
  than a phase enum would.
- **F7 cursor pagination** — `GET /sessions/{id}/messages?limit=N&before_id=<id>`,
  paging backwards.
- **F8 session trees** — entries carry `parent_id`; `POST /sessions/{id}/branch`
  moves the leaf persistently.

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
