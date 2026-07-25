# Design Spec

This is the behavioral specification for this SDK. Behavior questions are
answered here, not by [openai-agents-python](https://github.com/openai/openai-agents-python).

**The rule:** when this document does not cover a case, decide, implement it,
and add the invariant here **in the same change**.

Status markers used below:

- ✅ implemented and stable
- 🚧 will change — the plan responsible is named in parentheses
- ❓ open — see [§6](#6-open-questions)

---

## 1. Scope

### 1.1 What this is

A Go SDK for building agents on the **OpenAI Responses API**. It began as a port
of openai-agents-python and shares its core concepts — agents, handoffs,
guardrails, sessions — but evolves independently. See
[migration_from_python.md](migration_from_python.md) if you are arriving from the
Python SDK, and [upstream_watch.md](upstream_watch.md) for what we have reviewed
from upstream.

### 1.2 Non-goals

| Not doing | Why |
|---|---|
| **Chat Completions API** | Internal item types *are* Responses types. Supporting a second wire format would degrade the whole message model to a lowest common denominator. |
| **Provider-hosted tools** (`web_search`, `file_search`, `code_interpreter`, `computer_use`, …) | `Tool` is a sealed interface; every tool is a locally executed `FunctionTool`. Hosted tools bind a tool to one backend. |
| **A multi-provider abstraction** | Implement the `Model` interface for any backend you like. The SDK itself only guarantees depth of correctness for Responses semantics. |
| **Model price or capability tables** | They change constantly and do not belong in an SDK. `Usage` exposes raw token counts; pricing is the caller's concern. |
| **Realtime and voice** | A different interaction model, out of scope. |
| **Graph orchestration as the multi-agent primitive** | Handoffs already cover "switch agent at runtime". Graph orchestration, if ever needed, layers *on top* — see [§5.1](#51-handoffs-stay-graph-orchestration-does-not-replace-them). |

---

## 2. Core invariants

### 2.0 Entry points ✅

`Run` returns `(RunStream, RunControl)`; `RunSync` returns `(*RunResult, error)`.
`ResumeRun` / `ResumeRunSync` are the same pair for a paused run.

- **A run executes on the consumer's goroutine.** Ranging the stream advances
  the loop; abandoning it stops the run where it stands. There is no producer
  goroutine to leak and no context that must be cancelled on early exit.
- **The result is the stream's terminal event** (`RunCompletedEvent`), emitted
  exactly once on a run that ends without error. A failing run ends with a
  non-nil error and emits no completion — an outcome can never reach one channel
  and be lost from another.
- **The only behavioral difference between the two entry points is the model
  call**: `Run` streams it so raw events reach the consumer, `RunSync` makes one
  blocking call. Everything else — guardrail timing, persistence points, hooks,
  tracing — is identical, because there is one loop.
- **A trace always closes**, including when the consumer abandons the stream. ✅
  The run executes inside the iterator, so `yield` returning false unwinds the
  loop and the deferred trace finish runs; there is no window in which nobody
  owns the trace. Every span it opened is finished and exported.

### 2.0b Option grouping ✅

`RunOptions` groups its fields by what they configure — `Model`,
`Conversation`, `Exec`, `Observe` — rather than listing them flat. The zero
value stays usable.

The grouping is not cosmetic. `Conversation` collects options that **constrain
each other**: a local `Session`, `UsePreviousResponseID` and `ConversationID`
are alternatives, not layers, and a run that combines a local session with
server-managed state is rejected. A flat list hid that.

### 2.1 The run loop ✅

A run consists of **turns**. One turn = **one model call** plus every side effect
it triggers (tool execution, handoff).

```
for turn := 1; ; turn++ {
    check budget (turns / tokens / deadline)        🚧 tokens+deadline: plan8 L5
    check ctx cancellation
    resolve model / instructions / prompt / tools / handoffs / output schema
    build model input
    (first turn) run input guardrails
    call the model
    classify the response into message / tool call / handoff call / reasoning /
    unknown (kept verbatim, see §2.1b)
    execute side effects (§2.2)
    persist (§2.5)
    decide: continue / final output / interrupt
}
```

**Termination conditions**, highest precedence first:

1. `ctx` cancelled → return `ctx.Err()` immediately.
2. Budget exhausted → 🚧 (plan3) call the model once more **without tools** so it
   can close out in prose. With that option disabled, return `*MaxTurnsError`.
3. HITL interruption → return a `RunResult` carrying `Interruptions` and `State`.
4. The model produced a final output → see [§2.3](#23-deciding-the-final-output-).

### 2.1b Items ✅

Every `RunItem` reports two things beyond its payload:

- **`Source()` — who produced it.** The zero value is the model.
  `IsExternal()` separates what came from outside the SDK (the model, the
  caller) from what the runner synthesized (a tool output, a handoff
  acknowledgement, an error handler's fallback). The runner uses it to decide
  whether history ends on a local item; a context provider uses it to avoid
  re-ingesting its own injections.

  This replaced a sentinel response id (`__fake_id__`) stamped on synthesized
  items, which every consumer that cared had to know and string-compare.
  Provenance is not an id.

- **`Display()` — the projection a renderer needs**, produced by the SDK, which
  knows the wire format. It is a **hint**: a consumer that ignores it entirely
  must still render correctly from the item's own fields. That is what keeps
  `ItemDisplay` free to gain fields without breaking anyone.

Both survive `RunState` serialization, so a resumed run reports the same
provenance and renders the same timeline as before the pause.

**An unknown output item is kept, never dropped.** ✅ A model output type this
SDK does not model becomes an `UnknownOutputItem` carrying the original bytes,
and goes back on the wire byte for byte on the next turn. Dropping it is not
"ignoring a feature" — the next turn resends a history the model does not
recognize as its own.

The same rule reaches storage: `UnmarshalInputItem` accepts a typed item the
union does not know and preserves its bytes, so a session written by a newer
build stays readable. An item with no `type` is still rejected, so malformed
JSON does not slip through as an opaque blob.

### 2.2 Ordering within a turn ✅

**This is the most important invariant in this document.** The steps may not be
reordered.

| # | Step | Constraint |
|---|---|---|
| 1 | Publish `RunContext.TurnInput` | ✅ Set once the turn's input is final (before the model call), refreshed if `CallModelInputFilter` edits it. It is **what was actually sent** — under server-managed conversation state that is the new items only, not the whole history |
| 2 | On resume: drop already-completed sibling calls | Prevents duplicated side effects and a second `function_call_output` for the same call id |
| 3 | Partition calls by approval: `toRun` / `interruptions` / `rejected` | — |
| 4 | **If any call needs approval, pause the whole turn — no tool runs** | Deliberately unlike Python, which only pauses the gated calls. Rationale: `RunState` never holds partial results |
| 5 | Run `toRun` concurrently, then merge with `rejected` in **original call order** | Result order is deterministic and independent of completion order |
| 6 | A nested agent-as-tool interruption pauses the parent run too | Completed siblings **keep** their outputs; the interrupted call's output is **withheld** |
| 7 | Unknown tool → feed back `Tool 'X' not found.` | Only under `ToolNotFoundReturnToModel`; otherwise it is a `*ModelBehaviorError` |
| 8 | **Handoffs win**: switch to the target agent, end the turn | Tools in the same response have **already executed**; the final-output check is skipped |
| 9 | Decide the final output ([§2.3](#23-deciding-the-final-output-)) | — |

**Concurrency guarantees:**

- Tool concurrency is capped by `MaxToolConcurrency` (0 = unlimited). ✅
- A panicking tool is recovered and routed through that tool's error path. ✅
- When several tools fail, the error surfaced to the run is the one with the
  **lowest call index** — never whichever goroutine finished first. ✅
- 🚧 (plan4) a tool declaring `SequentialTool` forces the whole batch to run serially.

### 2.3 Deciding the final output ✅

Once the turn has no remaining tool work:

```
no message, but there was tool activity (e.g. all calls rejected)
    → continue to the next turn (results must reach the model)

message contains a refusal
    → *ModelRefusalError
      (a refusal wins over any text or structured content in the same message)

the agent has an OutputType:
    text present → parse against the schema
                   parse failure → InvalidFinalOutput recovery handler,
                                   or *ModelBehaviorError if none
    no text      → recovery handler, or **continue to the next turn**
                   (never a hard failure)

otherwise (plain text)
    → the message text is the final output (possibly the empty string)
```

### 2.4 Handoffs ✅

- A handoff is expressed as a **function call**; to the model it is just a tool.
- Multiple handoffs in one response → the **first** wins, the rest are ignored.
- Handoff alongside regular tools → **all tools execute first**, then the agent
  switches (step 8 of [§2.2](#22-ordering-within-a-turn-)).
- `MaxTurns` **keeps accumulating** across a handoff; it is not reset.
- `InputFilter` may rewrite the history handed to the target agent. **The session
  always retains the unfiltered conversation.**
- The target agent's `OnStart` fires at the beginning of the next turn.
- A handoff happens **inside the same run**, so it shares the run's session and
  usage. Contrast with agent-as-tool ([§2.8](#28-nested-agent-as-tool-attribution-)),
  which starts a nested run.

### 2.5 Session persistence boundaries ✅

| When | What is written |
|---|---|
| Just before the first model call | The new user input — deferred so a failure ahead of that leaves no orphan message ✅ |
| End of each turn | The items produced by that turn |
| Final turn | **After output guardrails pass** — a tripped final output is never persisted |

Whether a tripped input guardrail leaves the user message behind is decided by
`Blocking`, and by nothing else: ✅ a blocking guardrail finishes before the
save and before the model is reached, so a tripwire leaves the session
untouched and costs nothing; a racing one (the default) trips while the model
call is in flight, so the input is persisted and the request was made. Both
entry points answer identically.

**Core invariant — `safePersistBoundary`:** the stored conversation never
contains a function call without its output. When a run pauses for approval, the
pending `function_call` items are **withheld** and written together with their
outputs after resume.

🚧 (plan5) this guarantee does not survive an abnormal process exit; a
`RecoveryPolicy` repairs dangling state when the session is reopened.

**Entries are append-only.** 🚧 (plan1 §4.7) An entry's display may need
updating long after the turn that produced it has ended — a background task
card, a late diagnostic. That is expressed as a **new update entry** naming its
target, folded in at projection time; entries are never rewritten in place.
Multiple updates to one target merge in sequence order. An update whose target
does not exist is ignored, not an error — the target may have been folded away
by compaction.

### 2.6 Guardrails ✅

One `Guardrail` type covers every stage. Placement decides scope: guardrails in
`RunOptions` or on an `Agent` apply to the whole run — their tool stages cover
every tool that agent exposes — while guardrails on a `FunctionTool` apply to
that tool only.

| Stage | When | Decision space |
|---|---|---|
| `input` | First turn, before the model call (`Blocking`) or concurrently with it (default) | Allow / Replace / Trip |
| `output` | After the final output is produced, before persistence | Allow / Replace / Trip |
| `tool_input` | After arguments are parsed, **before tool lifecycle callbacks**, before execution | Allow / Replace / Trip |
| `tool_output` | After the tool runs, before the result is fed back | Allow / Replace / Trip |

**Ordering, concurrency and cancellation:**

- Input and output stages run their guardrails **concurrently** and fail fast:
  the first tripwire or error ends the wait and cancels the context handed to
  the rest.
- Tool stages run **in order** and stop at the first `Replace` or `Trip` — once
  one guardrail has substituted the content, running the others against the
  original is meaningless.
- A non-`Blocking` input guardrail runs concurrently with the model call. A
  tripwire **cancels the in-flight model call**: it is not billed and produces no
  response event.
- A panicking guardrail is recovered into an error that aborts the run — it never
  crashes the process.
- Every consulted guardrail produces a result, allowing decisions included, so
  callers can read each one's diagnostic payload.

**`Replace` semantics:** the decision's `Message` replaces the inspected content.

- `input` — appended as a single user text message replacing the original input.
  For finer rewriting use the model-input middleware (🚧 plan3) instead.
- `output` — replaces the final output value.
- `tool_input` — the tool does not execute; `Message` becomes its result.
- `tool_output` — replaces the content returned to the model.

🚧 (plan3) streaming currently runs input guardrails synchronously rather than
concurrently. Unifying the streaming and blocking APIs removes this difference;
the target is one behavior — concurrent, with cancellation.

### 2.7 Tools

#### Return values ✅ 🚧

🚧 (plan4) return values become
`ToolResult{Content, Details, Display, Usage, AddedTools, Terminate}`.

What the model sees:

| The tool returns | The model sees |
|---|---|
| `string` | verbatim |
| `nil` | `""` |
| `ToolOutputContent` (text / image / file) | native multimodal content items |
| anything else | JSON encoded |
| a value that cannot be JSON encoded | `fmt.Sprintf("%v")` — degraded, never dropped |

An empty result with no error is a **success with no output**, not a failure.

#### Errors ✅

- A tool returning an error goes through `FailureErrorFunction`, which turns it
  into model-readable text fed back to the model. This is the default.
- `FailureErrorFunction == nil` makes tool errors abort the run.
- A tool panic follows the same path, with the stack attached.
- Malformed argument JSON gets dedicated wording that prompts the model to resend
  valid JSON.
- 🚧 (plan3) N consecutive turns in which every tool failed trips a circuit
  breaker and aborts the run.

#### Approval ✅

- `NeedsApproval` / `NeedsApprovalFunc` decide; the function takes precedence.
- If **any** call in a turn needs approval, the whole turn pauses
  (step 4 of [§2.2](#22-ordering-within-a-turn-)).
- Approval decisions may be scoped ("this call", "all calls to this tool", …);
  the caller expresses the scope on the `RunState`.

### 2.8 Nested agent-as-tool attribution ✅

| Aspect | Attribution |
|---|---|
| **Usage** | Folded into the parent run's `Usage` |
| **Trace** | Nested spans join the parent trace, parented by the function span that triggered them |
| **Session** | **Not** shared with the parent by default; set `AgentToolConfig.Session` to opt in |
| **Interruptions** | Propagate upward as the parent's own; nested `RunState` is cached on the parent `RunState` keyed by call id |
| **Guardrail results** | The nested run runs its own guardrails; results stay on the nested result and are **not** merged into the parent `RunResult` |

An agent used as both a handoff target and an `AsTool` target follows whichever
path invoked it — handoff shares the run (and its session), agent-as-tool starts
a nested run (with its own session unless configured otherwise). The two paths do
not interact.

### 2.9 Budgets 🚧

🚧 (plan8 L5). The three dimensions are **OR**-ed: whichever trips first stops
the run.

| Dimension | How it is counted |
|---|---|
| `MaxTurns` | **Model calls.** Not reset by handoffs. A HITL resume continues accumulating. |
| `MaxTokens` | Cumulative `Usage.TotalTokens`. Nested agent-as-tool usage **counts**, because it folds into the parent `Usage`. |
| `Deadline` | A `time.Duration` measured from the start of the run. |

🚧 (plan2) LLM calls made by compaction itself **count toward `MaxTokens`** but
**not toward `MaxTurns`**.

When a budget trips mid-turn, the current tool batch is allowed to finish before
the run stops. Stopping mid-batch would leave dangling calls, which
[§2.5](#25-session-persistence-boundaries-) forbids.

### 2.10 Errors and recovery ✅ 🚧 (plan3 middleware only)

- Every SDK error carries a stable `ErrorCode`, read with `CodeOf(err)`. ✅
  `CodeOf` unwraps `%w` chains, so a code survives the run loop's own wrapping
  and a transport can read it off whatever `Run` returned.
- **An SDK error type always classifies.** ✅ The constructors set `Code`, and
  `CodeOf` falls back to the concrete type — an exported error built as a
  struct literal still reports its code rather than degrading to `CodeUnknown`.
- `Classify(code, err)` tags an error **without hiding it**: `errors.Is` and
  `errors.As` still reach the original. ✅ It is how packages outside the run
  loop (`sandbox`, `mcp`, custom tools) contribute a code.
- **The innermost classification wins.** ✅ `Classify` returns an
  already-coded error unchanged, so a boundary cannot overwrite a more specific
  reason with its own generic one.
- The code set is **open**. ✅ A consumer that does not recognize a code falls
  back to generic handling; this is what lets the SDK add one without a
  coordinated release downstream.
- Errors that a tool returns as *text* (the sandbox file and patch tools) are
  model-facing results, not failures, and carry no code. ✅
- Submodule errors (`sandbox`, `sessions`, `mcp`, `skills`) are classified **at
  the module boundary**, not deep inside. ✅
- Recoverable failures are handled by error handlers (max turns, model refusal,
  invalid final output). 🚧 (plan3) these become middleware.
- A fallback message synthesized by a recovery handler is tagged
  🚧 (plan1) `Source{Type: SourceErrorHandler}`.

---

### 2.11 Event fan-out ✅

One producer's events reach many independent consumers through `Fanout[T]`.

- **Publishing never blocks on a consumer.** A subscriber that cannot keep up
  loses items rather than stalling the producer or its peers.
- **A dropped item is always reported.** The next delivery on that subscriber's
  stream is preceded by a `*GapError` naming the range it lost. Silent loss is
  not an option the API offers: a consumer cannot distinguish a timeline missing
  content from one that never had it.
- **Sequence numbers are monotonic and assigned atomically with delivery**, so a
  subscriber never observes a higher number before a lower one — including when
  several goroutines publish concurrently.
- **A subscriber's replay backlog precedes anything published after it
  attached.** Registration and backlog delivery are one atomic step.
- `Close` means "nothing more will be published", not "discard what you have":
  already-buffered items are still delivered.

Rejected alternatives, both worse: dropping silently (corrupts the consumer's
view undetectably) and disconnecting the slow subscriber (turns a recoverable
hiccup into a visible failure).

---

## 3. Capabilities deliberately not provided

Beyond the non-goals in [§1.2](#12-non-goals):

| Not provided | Why |
|---|---|
| A built-in default model | The SDK does not guess which model you want. With none configured, `GetModel` returns a `*UserError`. |
| Implicit model-parameter injection (e.g. reasoning defaults for a model family) | Explicit beats implicit. Set `ModelSettings` yourself. |
| A free-form request passthrough dict | `ExtraBody` / `ExtraHeaders` / `ExtraQuery` cover it, and they are typed. |
| Redis / encrypted session backends | Implement the session storage interface. The SDK ships in-memory, JSONL and SQL. |
| A REPL and graph visualization | Not an SDK concern. |

---

## 4. Reference behavior you can rely on

Defaults that callers may depend on:

| Setting | Default | Note |
|---|---|---|
| `MaxTurns` | 10 | `MaxTurnsUnlimited` (-1) disables it |
| Strict schemas | on | `FunctionTool.Strict = false` relaxes both the advertised schema and local validation |
| Tool errors | fed back to the model | `DefaultToolErrorFunction`; set the field to `nil` to make them fatal |
| Tool concurrency | unlimited | Bound with `MaxToolConcurrency` |
| Input guardrails | concurrent with the model call | `Blocking: true` makes one a gate |
| Session persistence | after each turn | Final turn is written after output guardrails pass |

---

## 5. Recorded design decisions

These have been discussed and settled. Read the rationale before reopening.

### 5.1 Handoffs stay; graph orchestration does not replace them

A handoff is "switch agent at runtime"; a graph is "declare the topology up
front". They solve different problems. Our handoffs carry an `InputFilter` and
history folding; the equivalent in a graph model takes a lot of glue. Graph
orchestration, if it ever arrives, belongs *above* handoffs — serving task
orchestration, not replacing agent switching.

### 5.2 Type names stay

`RunItem`, `RunResult`, `RunContext` and friends came from the Python SDK, but
they read fine as Go. Renaming buys nothing except "looks less like Python" and
breaks every caller.

### 5.3 `Instructions` and `Prompt` both stay

`Prompt` (a server-stored prompt template with a version and variables) is a
**Responses API capability**, not a porting artifact. The two compose: a stored
prompt provides the base, instructions append to it.

### 5.4 The `Tool` interface is sealed

An unexported marker method keeps the set of tool kinds closed to the package.
This is how the "no hosted tools" decision is enforced, not an oversight.

### 5.5 Internal item types are Responses wire types

Zero conversion, zero information loss — reasoning ids, `encrypted_content` and
strict schemas all survive round-trips. The cost is that non-LLM entries need a
🚧 (plan1) `SessionEntry` wrapper to have somewhere to live.

### 5.6 Background work runs in-process, not in isolated processes

Background sub-agents ("tasks") run as **nested runs inside the same process**,
each with its own hidden session, reporting back by injecting a notification
message into the parent session.

The alternative — supervising one OS process per session and talking to it over
a line protocol — was considered and **rejected**. It buys crash isolation and
independent working directories at the cost of IPC, serialization, and a second
lifecycle to manage. Nested runs already give us independent sessions and
configuration; the isolation is not worth the machinery at this scale.

### 5.6b Tracing stays vendor-neutral; OpenTelemetry is a separate module

The core `tracing` package has no dependencies: a span is a flat record with
string ids and a `Data` map. `tracing/otel` translates that into OTel spans and
carries the OTel SDK, per §5.7.

The reconstruction is not free — our spans are exported after they finish, often
children first, while OTel builds trees from live spans through a context. It
works by pinning a custom `IDGenerator` to the ids the span already has. Two
invariants fall out and must hold:

- **`tracing.NewSpanID` is 8 bytes and `NewTraceID` is 16** — the OTel widths.
  Widening either would force every OTel-shaped exporter to truncate, silently
  and inconsistently.
- **The exporter requires a batch processor.** Pinning is stateful, so `Export`
  serializes; it is not a synchronous per-span processor.

The alternative — making the core emit OTel spans directly — was rejected:
it puts a heavy, fast-moving dependency in every consumer's build for a feature
most do not use.

### 5.7 A submodule exists only to keep a heavy dependency out of the core

The repository is a Go workspace with a root module (the SDK) plus submodules.
The **only** reason to split something into its own module is that it would
otherwise pull a heavy dependency into the core. Test helpers, small utilities
and anything dependency-free stay in the root module regardless of how
self-contained they are.

### 5.8 Public API compatibility begins at v0.2.0

Versions before v0.2.0 make no compatibility promise: the API is still being
shaped. From v0.2.0 onward, breaking changes to exported identifiers go through
a deprecation cycle.

---

## 6. Open questions

*(none currently — all questions raised during the independent-evolution review
have been settled into §2 above.)*

When a new case comes up that this document does not answer, add it here with
the options under consideration. Implementing it means moving it out of this
section and into §2 in the same change.

---

## 7. Change rules

1. Any PR that changes behavior described here **must update this document in
   the same change**.
2. A new §6 entry does not need an immediate answer, but the PR that implements
   it must move it out of §6 first.
3. Upstream changes are tracked in [upstream_watch.md](upstream_watch.md) with
   **no obligation to match**.
4. Users migrating from the Python SDK: see
   [migration_from_python.md](migration_from_python.md).
