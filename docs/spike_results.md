# Design spikes

Things measured before they were decided. Each entry states the question, what
was run, what came back, and what the design does about it.

[spec.md §5](spec.md#5-recorded-design-decisions) records the decisions
themselves; this records the evidence under them — the numbers a one-line
rationale cannot carry. Two of these came back against the intended design and
changed it (S2, S6), which is the reason for measuring first.

Run date: 2026-07-25. Sources: `scratchpad/spikes/` (throwaway module, not part
of the repo).

---

## Summary

| # | Question | Result |
|---|---|---|
| S1 | `param.Override` through the real request path | ✅ **PASS** |
| S2 | `RunStream` pull-model backpressure | ⚠️ **PASS with correction** — the existing design is *also* coupled |
| S3 | JSON Schema validation dependency | ✅ **PASS, zero new dependency** |
| S4 | OTel parent/child reconstruction | ✅ **PASS with a required technique** |
| S5 | Tree `PathToRoot` cost | ✅ **PASS** |
| S6 | Decorator capability lookup | ❌ **FAIL as designed** — design corrected |
| S7 | Unknown output item handling | ✅ **Confirmed silent drop**; detection is simpler than expected |

**Two design changes fall out of this:** S6 (decorators need an `Unwrap` chain)
and S2 (fan-out is required regardless of which stream model we pick).

---

## S1 — `param.Override` through the real request path ✅

**Question:** preserving an unknown item relies on `param.Override[ResponseInputItemUnionParam](json.RawMessage)`
to round-trip unknown items. Earlier verification only checked `json.Marshal`,
not a real openai-go request.

**Method:** `httptest` server standing in for the Responses endpoint; send a
mixed input list (normal message + overridden unknown item); inspect the bytes
the client actually wrote.

**Result:**

```
items sent: 2
  [0] {"content":"hello","role":"user"}
  [1] {"type":"some_future_call","id":"fc_1","weird":{"a":[1,2]},"status":"completed"}
PASS — unknown item round-tripped byte-for-byte through the real request path
```

**Conclusion:** the approach stands. Unknown items can be preserved
losslessly, not just logged.

---

## S2 — Pull-model backpressure ⚠️

**Question:** replacing the `chan(64)` + goroutine bridge with
`iter.Seq2`. Does a slow consumer stall the run loop?

**Method:** 500 events, 200 µs of producer work per event (ideal wall clock
100 ms), 2 ms of consumer work per event (a slow WS write). Compare three
transports.

**Result:**

| Transport | Wall clock | Note |
|---|---|---|
| `iter.Seq2` + slow consumer | 1.305 s (13.1× ideal) | producer fully coupled |
| `chan(64)` + slow consumer | 1.141 s, **producer finished at 992 ms** | 64 events of slack, then coupled |
| Fan-out, per-subscriber buffer + drop | 704 ms, producer decoupled | 314/500 delivered at buffer 256 |

**Correction to the plan:** the risk register said the pull model would *lose*
the isolation that `chan(64)` provides. It measurably does not — **the current
design is already coupled**, it just stalls 64 events later. The pull model is
not a regression.

**Conclusion:**

1. The pull model may proceed; it does not make backpressure worse.
2. A buffered fan-out with an explicit slow-subscriber policy is required
   **either way**. This is the `Fanout` decorator proposed in
   a fan-out broadcaster — promoted from "nice to have" to **prerequisite**.
3. The drop-vs-disconnect policy needs a decision (37 % drop at buffer 256 in
   this synthetic worst case is unacceptable for a chat UI). Absolute numbers
   here are synthetic; the structural finding is what matters.

---

## S3 — JSON Schema validation dependency ✅

**Question:** full JSON Schema validation (nested `required`,
default application). Is `google/jsonschema-go` an acceptable new dependency?

**Finding:** it is **not a new dependency**. `github.com/google/jsonschema-go
v0.4.3` is already a direct requirement in the root `go.mod`, used by
`agents/schema.go` for schema *generation*. We simply never used its validator.

Library size: 3,350 lines, one dependency (`go-cmp`, already present).

**Capability check** against a schema our own `SchemaFor[T]` generates:

```
root required missing    → REJECTED: required: missing properties: ["name"]
NESTED required missing  → REJECTED: validating /properties/inner: missing properties: ["port"]
nested type mismatch     → REJECTED: validating /properties/inner/properties/port:
                                     type: not-an-int has type "string", want "integer"
valid                    → accepted
ApplyDefaults({"b":7})   → {"a":"dflt","b":7}
```

**Conclusion:** much cheaper than estimated — no dependency
decision, no fallback plan needed. The known gap "nested required not
validated" is fixable with `Resolve()` + `Validate()`, and default application
comes free. Error messages already carry JSON-pointer paths, suitable for
feeding back to the model.

---

## S4 — OTel parent/child reconstruction ✅

**Question:** our spans are flat records with string IDs, exported in batch
after they have already finished — often children before parents. Can an OTel
exporter rebuild the tree?

**First attempt (naive):** inject a remote parent `SpanContext` for children,
start root spans with a bare context. Children linked correctly, but **the root
span got a freshly generated trace ID**, splitting the trace.

**Working approach:** a `TracerProvider` with a custom `IDGenerator` that is
pinned to the exact IDs of the span about to be created, plus an injected remote
parent `SpanContext`:

```
execute_tool   trace=0af765…319c span=b7ad6b7169203331 parent=b9c7c989f97918e1 ✓
chat           trace=0af765…319c span=b7ad6b7169203332 parent=b9c7c989f97918e1 ✓
invoke_agent   trace=0af765…319c span=b9c7c989f97918e1 parent=(root)           ✓
```

Trace IDs, span IDs, parent links and explicit timestamps all survive, with
children exported before their already-finished parent.

**Constraints for the implementation:**

- The exporter must serialize span creation (pin → `Start` → `End`) — a mutex
  around the translation loop. Acceptable for a batch exporter.
- **ID width mismatch:** `NewTraceID()` is `randHex(16)` = 16 bytes ✓, but
  `NewSpanID()` is `randHex(12)` = 12 bytes while OTel span IDs are 8 bytes.
  Either narrow `NewSpanID` to `randHex(8)` or truncate in the exporter.
  Narrowing at the source is cleaner and costs nothing.

**Conclusion:** the approach stands; the core stays vendor-neutral and
`tracing/otel` is a viable separate module. Add the two constraints above to the
implementation notes.

---

## S5 — Tree `PathToRoot` cost ✅

**Question:** moving sessions from a linear list to a tree. Is walking from
leaf to root affordable on a JSONL backend?

**Method:** synthetic sessions with a mostly-linear chain plus an abandoned
3-entry branch every 500 entries. Measure a naive per-call read+parse+walk
against loading an index once and walking it.

| Entries | File | Naive (read+parse+walk) | Index load | Index walk |
|---|---|---|---|---|
| 1,000 | 0.2 MB | 2.1 ms | 2.5 ms | **40 µs** |
| 10,000 | 2.3 MB | 16.7 ms | 14.1 ms | **336 µs** |
| 50,000 | 11.7 MB | 65 ms | 60.3 ms | **3.9 ms** |

**Conclusion:** the tree walk itself is free; the cost is parsing the file. The
JSONL backend must keep an in-memory index loaded once per open rather than
re-reading per turn — which it must do anyway to answer `getEntry(id)`.

Branching adds nothing measurable. **The performance objection to the tree model
does not hold**; the tree can be done now.

---

## S6 — Decorator capability lookup ❌ → design corrected

**Question:** replacing `FunctionTool`'s 15 fields with side
interfaces (`ApprovalRequiredTool`, `TimeoutTool`, …) queried by type assertion,
plus `WithXxx` decorators that embed the `Tool` interface. Does the assertion
pass through stacked decorators?

**Result: no.** Embedding the `Tool` *interface* promotes only `Tool`'s method
set. The outer type does not expose the inner decorator's methods:

```
WithTimeout(WithApproval(base)).(ApprovalRequiredTool) → false   ← BROKEN
```

Every stacking order loses every capability except the outermost one. **The
§4.2 as written does not work.**

**Corrected design** — an `errors.As`-style unwrap chain:

```go
type Unwrapper interface{ Unwrap() Tool }

// ToolAs finds the first implementation of T in the decorator chain.
func ToolAs[T any](t Tool) (T, bool) {
    for t != nil {
        if v, ok := t.(T); ok { return v, true }
        u, ok := t.(Unwrapper)
        if !ok { break }
        t = u.Unwrap()
    }
    var zero T
    return zero, false
}
```

Every decorator embeds a shared 3-line `deco` struct providing
`ToolName`/`isTool`/`Unwrap`. Verified across all stacking orders:

```
WithApproval(base)                              approval=true  timeout=false sequential=false ✓
WithTimeout(WithApproval(base))                 approval=true  timeout=true  sequential=false ✓
WithApproval(WithTimeout(base))                 approval=true  timeout=true  sequential=false ✓
WithSequential(WithTimeout(WithApproval(base))) approval=true  timeout=true  sequential=true  ✓
```

**Required change:** capability lookup goes through `ToolAs[T]`,
**never a bare type assertion**. This must be stated as an invariant — a bare
assertion compiles and silently returns false.

---

## S7 — Unknown output items today ✅

**Question:** are unknown model output items silently dropped today?
Confirm, and settle how to detect "no union variant matched".

**Result — `OutputToInput` round-trip:**

| Item type | Outcome |
|---|---|
| `message` | lossless |
| `function_call` | semantically identical (key order differs) |
| `compaction` | round-trips fine |
| unknown future type | **`ERROR: apijson: was not able to find discriminated union variant`** |

**Two findings:**

1. **`compaction` items are not dropped.** They enter the session through
   `OutputToInput` from `responses.compact`, never through
   `processModelResponse`. The concern is a non-issue.
2. **Unknown items are dropped by the classifier**, before `OutputToInput` ever
   sees them — `run_step.go`'s `default:` branch discards the item, so the
   conversion error above is never reached in practice.

**Detection is simpler than expected.** A type whitelist was proposed to
recognize "no variant matched". Two cheaper signals exist:

- `OutputToInput` already returns a clear, specific error.
- `json.Unmarshal` into `TResponseInputItem` for an unknown type yields a value
  that marshals to `null` — a reliable, allocation-free check.

**Conclusion:** implement `outputItemToInput` as "typed decode first; on failure
fall back to `param.Override`". No whitelist to maintain.

---

## Not run

| Item | Why | Plan |
|---|---|---|
| Token estimation calibration | Needs a real API key and real sessions | Do during implementation |
| Responses 400 overflow error shape | Needs a real API key | Same |
| `Collect()` vs tracing span lifetime | Design question, not empirical: the trace must finish when the terminal event is yielded, not when `Collect` returns, so a consumer that abandons the stream still closes the trace | Settled in §2.0 |
| `Emit` interaction with `RunStream` | Resolved by S2: tool progress goes into the same buffered fan-out; no separate mechanism | — |
