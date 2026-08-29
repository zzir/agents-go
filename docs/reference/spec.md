# Design Spec

This is the behavioral specification for this SDK: **what is always true**.
Behavior questions are answered here, not by
[openai-agents-python](https://github.com/openai/openai-agents-python).

**The rule:** when this document does not cover a case, decide, implement it,
and add the invariant here **in the same change**.

Every invariant below is implemented and stable unless flagged:

- 🚧 specified but not implemented yet
- ❓ open — see [§6](#6-open-questions)

Two companions carry what this document deliberately leaves out. *Why* an
invariant is what it is lives in
[design decisions](../explanation/decisions.md) (§5); what the project does
**not** do lives in [scope](../explanation/scope.md) (§1.2, §3). Section
numbers are permanent addresses across all three — a number is never reused
or renumbered.

---

## 1. Scope

Non-goals (§1.2) moved to [scope](../explanation/scope.md#12-non-goals).

### 1.1 What this is

A Go SDK for building agents on the **OpenAI Responses API**. It began as a port
of openai-agents-python and shares its core concepts — agents, handoffs,
guardrails, sessions — but evolves independently. See
[migration_from_python.md](../explanation/migration_from_python.md) if you are arriving from the
Python SDK, and [upstream_watch.md](../explanation/upstream_watch.md) for what we have reviewed
from upstream.

## 2. Core invariants

**How to read this section.** A bulleted claim in **bold** is normative — it is
the invariant, and code must satisfy it. The prose that follows a bold claim
explains or bounds it; it is not a second rule. Section numbers are permanent
addresses (code comments cite `spec §2.7f`), so a subsection is never
renumbered — which is why the letters run out of alphabetical order in places.

| § | Subsection | What it settles |
|---|---|---|
| [§2.0](#20-entry-points) | Entry points | `Run` / `RunSync` / `Resume*`; a run executes on the consumer's goroutine |
| [§2.0b](#20b-option-grouping) | Option grouping | `RunOptions` fields group by what they configure |
| [§2.1](#21-the-run-loop) | The run loop | One turn = one model call plus every side effect it triggers |
| [§2.1b](#21b-items) | Items | `RunItem` is one struct with a `Kind`, not an interface |
| [§2.2](#22-ordering-within-a-turn) | Ordering within a turn | **The ordering contract for a turn** — the most load-bearing invariant here |
| [§2.3](#23-deciding-the-final-output) | Deciding the final output | Which of message / refusal / schema parse becomes the final output |
| [§2.3a](#23a-the-save-point) | The save point | The turn boundary, and the fixed order of its five steps |
| [§2.3b](#23b-turn-snapshots) | Turn snapshots | A turn reads a `TurnSnapshot`, never the live `Agent` |
| [§2.3c](#23c-stopping-early) | Stopping early | The two places a turn can be ended, and what each guarantees |
| [§2.4](#24-handoffs) | Handoffs | A handoff is a function call the runner intercepts |
| [§2.5](#25-session-persistence-boundaries) | Session persistence boundaries | When the session is written, and what goes in |
| [§2.5b](#25b-session-entries) | Session entries | A session stores entries, not bare Responses items |
| [§2.5c](#25c-session-layering) | Session layering | Storage / Session / Projector — three layers, split by what varies |
| [§2.5d](#25d-sessions-are-trees) | Sessions are trees | `ParentID` makes a session a tree; reads are walks |
| [§2.5e](#25e-session-lifecycles) | Session lifecycles | `SessionRepo` owns which sessions exist, apart from their contents |
| [§2.5e2](#25e2-the-entry-lifecycle-contract) | The entry lifecycle contract | What may happen to an entry after it is written — the append-only contract |
| [§2.5f](#25f-compaction) | Compaction | Compaction is run-level; a checkpoint is appended, never a rewrite |
| [§2.5g](#25g-context-overflow) | Context overflow | Overflow reacts where compaction predicted wrong |
| [§2.5h](#25h-crash-recovery) | Crash recovery | `session.Recover` repairs what a killed process left inconsistent |
| [§2.6](#26-guardrails) | Guardrails | One `Guardrail` type, four stages; placement decides scope |
| [§2.7](#27-tools) | Tools | Return values, execution, and the approval partition |
| [§2.7b](#27b-tool-results) | Tool results | `ToolResult` separates what the model sees from what the host sees |
| [§2.7c](#27c-tool-capabilities-are-fields) | Tool capabilities are fields | A tool's capabilities are fields on `*Tool`, not interfaces |
| [§2.7d](#27d-tool-loop-safety-valves) | Tool-loop safety valves | The loop's own failure modes, and the valves that bound them |
| [§2.7e](#27e-truncated-responses) | Truncated responses | A provider-truncated response, and what the run does with it |
| [§2.7f](#27f-usage-attribution) | Usage attribution | Exactly one entry per response carries that response's `Usage` |
| [§2.7h](#27h-schema-validation) | Schema validation | Where arguments, handoff input and structured output are validated |
| [§2.7i](#27i-progressive-tool-disclosure) | Progressive tool disclosure | `Deferred: true` withholds a tool until something reveals it |
| [§2.7g](#27g-tool-progress) | Tool progress | `ToolContext.Emit` streams a partial result to the consumer |
| [§2.7j](#27j-sandbox-command-policy) | Sandbox command policy | `Policy` filters commands before the approval gate |
| [§2.7k](#27k-persistent-shells) | Persistent shells | When `exec_command` may reuse a named shell |
| [§2.7l](#27l-sandbox-tool-argument-decoding) | Sandbox tool argument decoding | `exec_command` decodes its own arguments, leniently |
| [§2.7m](#27m-a-sandbox-reports-its-own-timeout-never-the-callers-ending) | A sandbox reports its own timeout, never the caller's ending | `TimedOut` means the sandbox killed it — never the caller's deadline |
| [§2.7n](#27n-a-sandboxs-environment-is-part-of-its-container-identity) | A sandbox's environment is part of its container identity | `Options.Env` reaches every command; changing it replaces the container |
| [§2.7o](#27o-a-docker-sandbox-runs-as-the-images-user-and-joins-no-network) | A docker sandbox runs as the image's user and joins no network | Empty `User` = the image's own user; empty `Network` = none |
| [§2.7p](#27p-stop-keeps-the-filesystem-and-promises-nothing-else) | Stop keeps the filesystem and promises nothing else | `Lifecycle.Stop` guarantees the tree, never the processes |
| [§2.7q](#27q-a-sandbox-makes-its-working-directory) | A sandbox makes its working directory | A stock image need not ship one |
| [§2.7r](#27r-a-published-port-is-bound-to-the-daemons-loopback-and-reaches-only-0000) | A published port is bound to the daemon's loopback, and reaches only 0.0.0.0 | Declared at create; a 127.0.0.1 listener is invisible through it |
| [§2.7s](#27s-apply_patch-locates-hunks-by-whole-lines) | apply_patch locates hunks by whole lines | Context can never bind inside a longer line |
| [§2.8](#28-nested-agent-as-tool-attribution) | Nested agent-as-tool attribution | How usage, spans and errors attribute across a nested agent-as-tool |
| [§2.9](#29-budgets-) | Budgets 🚧 | `MaxTurns` is the one budget dimension implemented |
| [§2.10](#210-errors-and-recovery) | Errors and recovery | Stable `ErrorCode`s, and which errors a run can recover from |
| [§2.11](#211-event-fan-out) | Event fan-out | `Fanout[T]`: one producer, many consumers, a drop is a `*GapError` |
| [§2.11b](#211b-run-control) | Run control | `RunControl` — steer, inject, approve, cancel, from another goroutine |
| [§2.11e](#211e-span-coverage) | Span coverage | Which spans are guaranteed, and their parent-child shape |
| [§2.11d](#211d-diagnostics) | Diagnostics | A `Diagnostic` records trouble a run survived |
| [§2.11c](#211c-logging) | Logging | Off by default; what the SDK logs when it is not |
| [§2.12](#212-middleware) | Middleware | `RunMiddleware` wraps a whole run, outermost first |
| [§2.13](#213-background-tasks) | Background tasks | A task is a sub-agent that outlives the turn that started it |

### 2.0 Entry points

`Run` returns `(RunStream, RunControl)`; `RunSync` returns `(*RunResult, error)`.
`ResumeRun` / `ResumeRunSync` are the same pair for a paused run.

- **A run executes on the consumer's goroutine.** Ranging the stream advances
  the loop; abandoning it stops the run where it stands. There is no producer
  goroutine to leak and no context that must be cancelled on early exit.
  The moment `emit` observes the departure it cancels the RUN'S OWN context
  root, so everything the turn has in flight — the model call, the tool batch
  the loop is blocked on, racing input guardrails — is told to stop rather
  than completing into a run nobody reads. Waiting instead parked the
  consumer's `break` for the work's full duration, and forever for a guardrail
  that returns only when cancelled; a tool that ignores its context still runs
  to completion, but its result is discarded. (One narrow exception to "the
  consumer's goroutine": a tool streaming progress yields from its own
  goroutine — [§2.7g](#27g-tool-progress).)
- **A `RunStream` is single-use.** Ranging it a second time yields a
  `*UserError` instead of anything else: the run body lives inside the
  iterator, so a second range would re-execute it — model billed again, tools
  re-running their side effects, the session taking duplicates — and it would
  do so silently.
- **The result is the stream's terminal event** (`RunCompletedEvent`), emitted
  exactly once on a run that ends without error. A failing run ends with a
  non-nil error and emits no completion — an outcome can never reach one channel
  and be lost from another.
- **The only behavioral difference between the two entry points is the model
  call**: `Run` streams it so raw events reach the consumer, `RunSync` makes one
  blocking call. Everything else — guardrail timing, persistence points, hooks,
  tracing — is identical, because there is one loop.
- **A trace always closes**, including when the consumer abandons the stream.
  The run executes inside the iterator, so `yield` returning false unwinds the
  loop and the deferred trace finish runs; there is no window in which nobody
  owns the trace. Every span it opened is finished and exported.

### 2.0b Option grouping

`RunOptions` groups its fields by what they configure — `Model`,
`Conversation`, `Exec`, `Compaction`, `Observe`, `Log` — rather than listing
them flat. The zero value stays usable.

The grouping is not cosmetic. `Conversation` collects options that **constrain
each other**: a local `Session`, `UsePreviousResponseID` and `ConversationID`
are alternatives, not layers, and a run that combines a local session with
server-managed state is rejected. A flat list hid that.

### 2.1 The run loop

A run consists of **turns**. One turn = **one model call** plus every side effect
it triggers (tool execution, handoff).

```
for turn := 1; ; turn++ {
    check turn budget
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

1. `ctx` cancelled → the run ends there, and `ctx.Err()` reaches the caller
   wrapped in a `*RunError` carrying the turns that did complete. A
   cancellation noticed inside the loop is a failure like any other; only
   failures from *before* the loop are returned bare.
2. Budget exhausted → `*MaxTurnsError`, unless `ToolLoop.FinalTurnWithoutTools`
   buys one last tool-free model call ([§2.7d](#27d-tool-loop-safety-valves)).
3. HITL interruption → return a `RunResult` carrying `Interruptions` and `State`.
4. The model produced a final output → see [§2.3](#23-deciding-the-final-output).

**A `RunState` round-trips whole.** Everything a resume consumes is in the
wire format — the pending injected input, the disclosed deferred tools, the
server-conversation cursor, the off-chain-history flag and the host extra map
(`Extra`) included — pinned by a full-field round-trip test
(`RunStateSchemaVersion` 1.6). The in-process resume passing the live pointer
must never be the only path that works; the serialized surface IS the
contract. The cursor in particular rides along so a resumed run keeps sending
deltas: the resumed turn re-processes a response the restored cursor already
accounts for and does not advance it — re-deriving the cursor there marked
pre-pause sibling tool outputs as already served, and a server-managed
conversation never received them.

**And it round-trips the run's full past, deliberately.** The serialized state
carries every raw response and generated item so far, so its size grows with
the run. That is the cost of a contract, not an oversight: a resumed run's
`RunResult` must report the same `RawResponses` (and therefore the same
`UsageByResponse()`) as one that never paused, and the max-turns handler's
snapshot promises "every response so far". Trimming the state to the
interrupted response alone would make pausing observable in the result.

**The run keeps one item log.** `RunResult.NewItems` and `RunState.SessionItems`
are that log in full, append-only. The model's view of it is a tail: a handoff
input filter ([§2.4](#24-handoffs)) or a recompaction ([§2.5f](#25f-compaction),
[§2.5g](#25g-context-overflow)) folds the log so far into the run input and
restarts the view at the log's end, so `RunState.GeneratedItems` is the suffix of
`SessionItems` the model still sees — a projection of the log, never a second
record, and a resume takes it as such (by length). There is no list an item can
be appended to and left out of the other.

**A `RunState` decodes across a version window, not on strict equality.**
`RunStateFromJSON` accepts the same schema major from
`runStateOldestDecodableMinor` up to `RunStateSchemaVersion`; anything newer,
any other major, and anything below the floor is a `*UserError` naming which
way it missed. A minor may only ADD fields — a bump that replaces or
reinterprets one must raise the floor to itself. See [§5.18](../explanation/decisions.md#518-a-runstate-decodes-across-a-version-window-and-the-window-is-earned).

### 2.1b Items

**`RunItem` is one struct with a `Kind`, not an interface.** The kinds are a
closed set the runner produces — message, tool call, tool output, handoff
call/output, reasoning, injected input, unknown — and a caller cannot add one,
which is the definition of a union, not of a polymorphic seam. A stored
`RunState` holds `{type, agent, input, source, display}`, and the struct IS
that shape, live and stored — serialization flattens nothing.

Consumers switch on `Kind` and must treat an unrecognized kind as opaque —
render it via `Display()`, never fail — so the set can grow without breaking
them.

Beyond its payload, every item reports two things:

- **`Source` — who produced it.** The zero value is the model.
  `IsExternal()` separates what came from outside the SDK (the model, the
  caller) from what the runner synthesized (a tool output, a handoff
  acknowledgement, an error handler's fallback). A context provider uses it to
  avoid re-ingesting its own injections, and the runner reads it to find the
  last model-produced item — the frontier of what a server-side response chain
  can hold ([§2.5f](#25f-compaction)). It does not settle that question on its
  own: input the caller injected after the last model call is external and is
  still off the chain.

  Provenance is a field, not a sentinel response id stamped on synthesized
  items that every consumer would have to know and string-compare.

- **`Display()` — the projection a renderer needs**, produced by the SDK, which
  knows the wire format. It is a **hint**: a consumer that ignores it entirely
  must still render correctly from the item's own fields. That is what keeps
  `ItemDisplay` free to gain fields without breaking anyone.

Both survive `RunState` serialization, so a resumed run reports the same
provenance and renders the same timeline as before the pause. A rebuilt item
carries its replayed input form (`RawInput`) and stored display; `Raw` is nil,
and so is `Output` — a tool's Go-native return value does not round-trip, only
its rendered input form does. A resume replays history from input items, which
is all it needs.

**An unknown output item is kept, never dropped.** A model output type this
SDK does not model becomes an `ItemUnknown` run item carrying the original bytes,
and goes back on the wire byte for byte on the next turn. Dropping it is not
"ignoring a feature" — the next turn resends a history the model does not
recognize as its own.

The same rule reaches storage: `UnmarshalInputItem` accepts a typed item the
union does not know and preserves its bytes, so a session written by a newer
build stays readable. An item with no `type` is still rejected, so malformed
JSON does not slip through as an opaque blob.

### 2.2 Ordering within a turn

**This is the most important invariant in this document.** The steps may not be
reordered.

| # | Step | Constraint |
|---|---|---|
| 1 | Publish `RunContext.TurnInput` | Set once the turn's input is final (before the model call), refreshed if `CallModelInputFilter` edits it. It is **what was actually sent** — under server-managed conversation state that is the new items only, not the whole history |
| 2 | On resume: drop already-completed sibling calls | Prevents duplicated side effects and a second `function_call_output` for the same call id |
| 3 | Partition calls by approval: `toRun` / `interruptions` / `rejected` | — |
| 4 | **If any call needs approval, pause the whole turn — no tool runs** | Pausing only the gated calls would leave `RunState` holding partial results |
| 5 | Run `toRun` concurrently, then merge with `rejected` in **original call order** | Result order is deterministic and independent of completion order |
| 6 | A nested agent-as-tool interruption pauses the parent run too | Completed siblings **keep** their outputs; the interrupted call's output is **withheld** |
| 7 | Unknown tool → feed back `Tool 'X' not found.` | Only under `ToolNotFoundReturnToModel`; otherwise it is a `*ModelBehaviorError` |
| 8 | **Handoffs win**: switch to the target agent, end the turn | Tools in the same response have **already executed**; the final-output check is skipped |
| 9 | Decide the final output ([§2.3](#23-deciding-the-final-output)) | — |

**Concurrency guarantees:**

- Tool concurrency is capped by `MaxToolConcurrency` (0 = unlimited).
- A panicking tool is recovered and routed through that tool's error path.
- When several tools fail, the error surfaced to the run is the one with the
  **lowest call index among non-cancellation errors** — never whichever
  goroutine finished first, and never a sibling's `context.Canceled` echo of
  the failure that cancelled it. Cancellation surfaces only when it is all
  there is (the consumer abandoned the run mid-batch).
- A tool declaring `Sequential` forces the whole batch to run serially.

### 2.3 Deciding the final output

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

### 2.3a The save point

The **save point** is the turn boundary: the turn's assistant message and every
tool result are persisted, and the next model call has not happened yet.

It is one place in the code, and its step order is the contract:

1. flush the turn to the session
2. ask `ShouldStopAfterTurn`
3. compact ([§2.5f](#25f-compaction)), rebuilding the context from the log
4. drain the steer and next-turn queues ([§2.11b](#211b-run-control))
5. call `PrepareNextTurn`

Persisting first is what makes the rest safe: a run that stops at step 2, or
whose context is rewritten at step 3, has its history already written. Asking
to stop before compacting means the decision is made against the turn that
actually happened rather than a shortened view of it. Draining after step 3,
never before, is what keeps injected input out of the pass that ran before it
arrived — compacted away in the same breath it was delivered.

A **handoff** reaches only step 1 and 2. The next turn belongs to a different
agent, so its snapshot is resolved fresh, and its context is about to be
rewritten by the handoff input filter.

### 2.3b Turn snapshots

A turn is resolved into a `TurnSnapshot` — agent, model, settings,
instructions, prompt, tools, handoffs, output schema, input — before the model
is called, and the turn reads the snapshot from then on rather than the agent.

- `PrepareNextTurn` may return a replacement, which applies to **one turn**;
  the turn after resolves afresh, so dynamic instructions still change per turn.
- It is how a run changes shape mid-flight (swap to a cheaper model, withdraw a
  tool once used) **without mutating the `Agent`**, which a concurrent run may
  be reading.
- **The runner owns `Input`.** A returned snapshot has it replaced with the
  next turn's real input. A prepared snapshot is nearly always a copy of the
  previous turn's, and honoring its `Input` would replay that turn with the
  tool call and its output missing — a silent corruption, since the run still
  looks like it is progressing. To edit what a call sends, use
  `ModelOptions.InputFilter`, which runs per turn on the input the loop built.

### 2.3c Stopping early

A turn that would otherwise continue can be ended from two places, and only
two:

| Level | Mechanism | Final output |
|---|---|---|
| tool | `ToolResult.Terminate` | the last tool's output |
| run | `ExecOptions.ShouldStopAfterTurn` | the turn's last message, else its last tool output |

- `Terminate` requires **unanimity** across the batch ([§2.7b](#27b-tool-results)).
- `ShouldStopAfterTurn` is consulted at the **save point** ([§2.3a](#23a-the-save-point)), at both branches that would take another turn, including a handoff. A run stopped there has its full
  history saved and needs no unwinding, and stopping at a handoff means control
  never leaves the agent.
- It is **not** consulted on a turn that already ends the run: asking whether to
  stop something that is stopping is noise.
- It is a **predicate, not a producer**. The final output is derived from the
  turn, so a stopped run's result cannot disagree with its saved history. A
  caller wanting something else computes it from `RunResult.NewItems`.
- The `*TurnResult` a hook is handed is **its own to read**. Writes to its
  fields reach neither the run nor the next hook, so a hook that clears
  `NewItems` can neither blank the stopped run's final output nor hide the
  turn from `PrepareNextTurn`.
- Both survive `ResumeRun`: an approved run carries the same stop policy, or it
  would sail past the point it was configured to stop at.

There is deliberately no agent-level early-stop configuration. Naming tools up
front cannot express anything the turn predicate cannot, and the policy belongs
to the run — the same agent gets reused across runs that stop at different
points.

### 2.4 Handoffs

- A handoff is expressed as a **function call**; to the model it is just a tool.
- On the stream it surfaces as **both** `tool_called` and `handoff_requested` —
  the model's view and the runner's. The `tool_called` wrapper carries
  `RunItem.IsHandoff = true`: it has no paired `tool_output` (the handoff
  switches agents instead of returning), and the flag is what lets a consumer
  drop or badge the wrapped form without keeping a list of every handoff tool
  name in the graph.
- The target resolves from `OnInvoke` when set — the runtime authority, free to
  pick an agent from the arguments — else from `Target`, the static declaration
  `HandoffTo` fills. Neither set fails the run with a `*UserError`. `Target` is
  what keeps the handoff graph statically enumerable: a consumer rebuilding an
  agent registry (for `RunStateFromJSON`, an approval UI) walks `Target` fields
  without invoking user code, and a dynamic handoff declares itself
  non-enumerable by leaving `Target` nil.
- Multiple handoffs in one response → the **first** wins, the rest are ignored.
- Handoff alongside regular tools → **all tools execute first**, then the agent
  switches (step 8 of [§2.2](#22-ordering-within-a-turn)).
- `MaxTurns` **keeps accumulating** across a handoff; it is not reset.
- `InputFilter` may rewrite the history handed to the target agent. **The session
  always retains the unfiltered conversation.**
- The target agent's `OnStart` fires at the beginning of the next turn.
- A handoff happens **inside the same run**, so it shares the run's session and
  usage. Contrast with agent-as-tool ([§2.8](#28-nested-agent-as-tool-attribution)),
  which starts a nested run.

### 2.5 Session persistence boundaries

| When | What is written |
|---|---|
| Just before the first model call | The new user input — deferred so a failure ahead of that leaves no orphan message |
| End of each turn | The items produced by that turn |
| Final turn | **After output guardrails pass** — a tripped final output is never persisted |

Whether a tripped input guardrail leaves the user message behind is decided by
`Blocking`, and by nothing else: a blocking guardrail finishes before the
save and before the model is reached, so a tripwire leaves the session
untouched and costs nothing; a racing one (the default) trips while the model
call is in flight, so the input is persisted and the request was made. Both
entry points answer identically.

A save that leaves nothing behind is announced on the stream as
`ItemsPersistedEvent`. The implication is **one-way**: the event guarantees
that every item the stream showed before it is in the store; its absence
promises nothing (a run without a session never emits it, history restored on
resume predates the stream, and a save that held items back — an
interruption's pending calls — stays silent, precisely because the stream has
shown items the store does not yet hold). Consumers mirror persisted state
from this event rather than inferring the SDK's persist timing from raw
response events.

**Core invariant — `safePersistBoundary`:** the stored conversation never
contains a function call without its output. When a run pauses for approval, the
pending `function_call` items are **withheld** and written together with their
outputs after resume.

This guarantee does not survive an abnormal process exit; a `RecoveryPolicy`
repairs dangling state when the session is reopened.

**Entries are append-only** — a display that settles late is a new update
entry folded in at read time, never a rewrite ([§2.5b](#25b-session-entries)).

### 2.5b Session entries

A session stores **entries**, not bare Responses items. An entry carries the
item plus what the run knew about it — provenance, display, the model call it
belongs to — or something that is not a Responses item at all (an annotation, a
compaction checkpoint, terminal output).

- **The kind vocabulary is open.** A build that meets a kind it does not know
  ignores the entry rather than failing, so a session written by a newer version
  stays readable.
- **Projection decides what the model reads.** Items and compaction checkpoints
  project by default; annotations, terminal output and custom entries do not.
  Recording something and putting it in the model's mouth are different acts.
  `RunOptions.Conversation.Projectors` overrides this per kind.
- **A compaction summary projects as a *system* message**, not a user one:
  nobody said it, and attributing it to the user would put words in their mouth.
- **A checkpoint copies nothing** — it names what it folded and carries only
  content that exists nowhere else ([§2.5f](#25f-compaction)).

**Entries are append-only.** Nothing is rewritten in place; that is what lets
a session be forked, shared and read concurrently without a writer invalidating
a reader's view. A display settled after its turn ended is expressed as an
**update entry** naming its target, folded in at read time:

- Updates apply in stored order; the last write wins per field.
- **An update may be stored before its target.** Association is by id, so the
  "the task finished before the parent turn was persisted" race does not need
  handling — it cannot occur.
- **A tool call is also addressable by its call id** (`TargetCallID`), for an
  amender that knows the call and not the entry. The entry id is assigned by
  storage at a moment the amender may not have reached yet; requiring it would
  put the look-it-up-and-retry race back that this mechanism exists to remove.
- **An update whose target is missing is ignored, not an error.** The target may
  have been folded away by compaction, and failing an entire read over a stale
  pointer would make history unloadable.
- **Folding is a projection: it never writes through to storage.** Readers
  get shallow copies whose `Display` (and its `Extra` map) is shared with the
  stored entry, so the fold copies what it merges instead of editing in
  place — a read must never change what the next read returns.

A server-managed conversation (`openai.ConversationsSession`) can hold only
items; other kinds are dropped on write, because failing a run over a UI
annotation that could not be stored server-side is worse than losing it.

### 2.5c Session layering

A session is three layers, split along what varies:

- **`session.Storage`** reads and writes entries and understands nothing about
  what they mean.
- **`Session`** is a concrete type, not an interface, that turns entries into
  the model's view. Storage varies; "how history becomes model input" does not,
  and as an interface every backend re-answered it and they drifted.
- **`SessionRepo`** owns lifecycles — create, open, list, delete.

**Reads page on sequence numbers, not offsets.** Entries keep arriving, so an
offset shifts under a concurrent append and a second page silently skips or
repeats. A negative `Cursor.Limit` takes the most recent N.

**Derived state is a fold, never a stored field.** `State` and `Stats`
recompute from the entries. A field maintained beside the log has to be updated
on every write and can disagree with it after a crash, a concurrent writer or a
fork; a fold cannot. `State` folds the ACTIVE BRANCH — the view recovery reads
— not append order: a dangling call on an abandoned attempt is not pending,
and folding every branch reported it forever as a stuck approval nothing could
clear. `Stats` stays whole-log, because it counts what is stored.

**`ContextEntries` is the active branch minus what compaction folded** — the
checkpoints themselves stay in the view (they carry the summary and stand-ins
the projection renders), while the entries their exclusions name are left out:
re-sending folded history would undo the compaction, and a cursor limit must
count entries the model will actually see. `ProjectEntries` applies the same
exclusions again wherever it is called, so a view built without the filter
still cannot replay folded history.

**A branch view is computed from the whole log, and the cursor only trims the
answer.** The tree is walked by following `ParentID` links back from the leaf,
which no backend can express as a range scan, so `ContextEntries` reads every
entry and pages the projection afterwards — a `Cursor` passed to it saves
nothing on the way in. A run reads once per turn, so the cost grows with the
conversation and compaction does not bring it down: compaction shrinks what the
model sees, not what the store holds. That is a known ceiling, accepted for now
because the alternative is pushing the walk into every backend. The way out, if
one is needed, is an optional capability that resolves an ancestor chain
server-side (a recursive CTE in SQL) rather than a second canonical view.

Capabilities a store may or may not have are **optional interfaces**, not
required methods: `AtomicReplacer`, `GuardedReplacer`, `CompactionAware`.
**A wrapper that claims a capability delivers its contract or refuses**: delegating `AtomicReplacer` to
a wrapped store without it must return an error before touching anything, never
degrade to a non-atomic Clear+Append — a caller type-asserted the interface
precisely to rule that failure mode out. `GuardedReplacer` is delegated the
same way: a wrapper over a store that cannot compare the log back errors rather
than answering `replaced=false`, which would assert the log had moved.

### 2.5d Sessions are trees

An entry names its parent, so a session is a walk rather than a pile.

- **Branching abandons without deleting.** Moving the active branch leaves the
  old attempt recorded and off the path; "try that again differently" costs
  nothing and loses nothing.
- **Switching branches is an append**, not a mutable pointer: `EntryKindLeaf`.
  That keeps the switch itself in the history, and lets the current leaf be
  **derived by folding the log** rather than stored beside it where it could
  disagree after a crash.
- **The model sees the active branch**, not append order. Sending an abandoned
  attempt would show a conversation that contradicts itself.
- **Parent links are assigned by storage**, which is the only layer that knows
  the ids it is about to mint. `PrepareAppend` is shared so every backend links
  identically — a store that got this wrong would read back as disconnected
  roots, which no single-append test would catch.
- **A linkless run of entries is one straight line, and the branch continues
  through it.** Entries carrying no parent ids and no leaf moves — a session
  from before branching existed, a server-held store with no tree — cannot be
  off-branch, since branching needs links. The walk to the leaf is extended
  over the linkless prefix ahead of its root, so the first LINKED append to
  such a session does not drop everything written before it. Every
  branch-scoped view (`ContextEntries`, `PathEntries`) shares this rule
  through one helper (`ActiveBranchOf`).
- **A walk does NOT stop at a compaction checkpoint.** The walk answers "which
  entries are on this branch", and a folded entry is still on the branch it was
  written to; what the model sees is projection's question. A walk stopped at
  a checkpoint would hide the kept entries from a branch-scoped reader while
  the model could still see them. A missing parent ends the walk (a
  filtered view may have dropped an ancestor), and a repeated id does too, so a
  corrupt session reads short instead of hanging.

**Fork extracts a branch; branch moves within one session.** A fork carries
entry ids across unchanged, so an update entry naming one still finds its
target. The destination is written through `session.ReplaceEntries`, so a
storage that can swap atomically (`AtomicReplacer`) never shows a
cleared-but-unfilled fork target when a failure lands mid-write.

### 2.5e Session lifecycles

A `SessionRepo` owns which sessions exist, separately from their contents.

- **Existence is recorded, not inferred.** A session created with no entries is
  still listable; inferring existence from contents makes "empty" and "never
  created" the same state.
- **`Hidden` belongs to the session**, not to each caller's filter. A session
  that serves another one (a background task's history) is excluded from
  listings by default, and is created naming the session it serves
  (`CreateOptions.ParentID`) so a repo that attaches facts to sessions — an
  owner — can inherit them instead of guessing. A repo may ignore the name or
  refuse an unknown one with `ErrNotFound`; it never creates the parent.
- **A backend may constrain the shape of the ids it accepts** — the server's
  PostgreSQL backend types every id column `uuid`, so a caller-chosen id must
  be one. The conformance suite routes its literal names through
  `RepoUnderTest.IDs` for such a backend; a backend without the constraint
  leaves it nil.
- **Opening an unknown session is an error**, never an empty session. A wrong id
  that reads as a fresh conversation makes a run start over instead of
  continuing, which is worse than failing.
- **Deleting removes the entries with it**, atomically where the backend can, so
  no entries survive pointing at a session that is gone.
- **A name that two ids share belongs to whichever id claimed it.** A backend
  that maps ids onto a namespace with fewer characters than ids have — a
  filesystem — must record the original id and refuse to open or delete through
  the other one. **A check that cannot be made refuses**: not being able to read
  who owns the name is a reason to stop, not to proceed.
- **A failed create leaves nothing behind.** A backend that claims a name before
  it can finish writing gives the claim back if the rest fails, or the id is
  burned: unusable and un-recreatable.

### 2.5e2 The entry lifecycle contract

Everything above describes what a session *is*. This describes what happens to
an entry over its life — minted, addressed, walked, removed — in one section,
because rules decided one at a time, in whichever backend a defect surfaces,
drift apart across implementations.

**The rule this section is really about: none of it is a backend's decision.**
Each item below names who implements it. Where that is "shared", a backend that
answers the question itself is a bug even if its answer is right, because the
next backend will answer differently.

#### Identity

- **A session id names a session, not a place.** Deleting an id and creating it
  again yields a session with storage of its own. A handle to the deleted one
  can neither read what its replacement writes nor write into what it reads.
  *Shared:* `session.Ref` is the address, `session.NewGeneration` mints the
  discriminator, and `session.NewSessionID` mints the id itself for a `Create`
  that supplied none. A function that takes a ref cannot be handed a bare
  id, which is the point — carrying the generation as a field beside the id
  made every hand-built handle, every resolve-by-id and every delete-by-name a
  chance to forget it, silently.
- **A handle is bound when it is BUILT**, never on first use. A handle created,
  held, and first touched after its session was deleted and recreated still
  refers to the one it was built for. *Shared.*
- **A constructor where the id names the STORAGE** (`sessions.New`) is a
  different thing and keeps its meaning: opening it twice
  is the same conversation, and it cannot tell "reopened" from "recreated". It
  does not share storage with a repo's sessions **in either direction** — a repo
  delete does not reach it, and its writes do not reach a repo session.
  *Per backend, since only some have one.*
- **A lookup that failed is not an answer.** "This id has no session" is a fact;
  a cancelled context or an unreachable store is a failure to look. Resolving
  the second to the first silently moves a handle into another scope. *Shared.*

#### Entry identity

- **An entry id is unique within its session and never handed out twice**,
  including after the entry holding it is gone. Minting from the stored entry
  count is therefore wrong: removing an entry frees its id for the next append.
  *Shared.*
- **Ids are opaque.** Nothing outside the minting code constructs or parses one.
  A caller that needs an entry reads its id back. *Contract on callers.*
- **A backend that can constrain them does**, so a collision is a failed write
  rather than two rows answering to one name. *Per backend.*

#### Sequence numbers

`Seq` is a **cursor position**, and that is the whole of its meaning.

- **Monotonic within a session**, so `AfterSeq` orders — and it is the ONLY
  order a backend reads history in. A storage row's own key may be
  time-ordered by construction (a UUIDv7), but a clock that steps backwards
  or a second process reorders such keys; `Seq` does not move.
- **Never reused**, including after the entry holding it is removed. A caller
  resuming from the last number it saw would otherwise skip the next append
  forever, silently, its cursor already past it.
- **Never moved for an entry that stays.** Numbering by position in a result set
  shifts every survivor whenever a read filters one out — a compaction pass, an
  item another model produced. A rewrite that RE-ADDS an entry is the one
  exception: server-side compaction carries over what it did not summarize,
  keeping the ids (an update entry finds its target by id) and taking fresh
  numbers. A consumer tailing with `AfterSeq` sees such an entry a second time
  under a new number, so it deduplicates by id — an (id, seq) pair is not fixed
  for a session's life.
- **`Clear` and `ReplaceEntries` do not restart it.** A cursor outlives the
  entries it pointed at, so a replaced history that renumbers from the beginning
  lands entirely before an existing cursor and is skipped in full.
- One value per entry, whichever API returns it. *All shared, but for the one
  backend below that holds no numbers of its own.*
- **A server-held conversation numbers best-effort.**
  `openai.ConversationsSession` has no local store to have allocated a number,
  so it numbers by position in what the server returned: if the server ever
  stops returning an item, everything after it shifts. Reading the most recent
  N (a negative limit) is unaffected; resuming from `AfterSeq` is best-effort
  there. *Per backend.*

#### The change record

- **Every change moves a session in its listing**, not just an append:
  clearing is a change.
- **It never moves backwards.** A backend that infers the time from stored
  content moves a session back to its creation as soon as there is no content
  left to infer from.
- **A session with no writes yet sorts by when it was created**, not by the zero
  time.
- **A session's metadata and the listing's are the same answer**, read through
  one path. *Shared.*
- **A listing is that record's order, newest first**, and `ListOptions.Limit`
  cuts it from the newest end, after the hidden filter — a background task's
  transcript must not eat a slot the caller asked for. A limit that is not
  positive is no limit. It is a plain count rather than a `Cursor`: a listing
  has no sequence number to resume from, and `Cursor`'s other reading of a
  negative limit — take the most recent N — belongs to entry cursors, which
  have an oldest end to take from. Sessions sharing a time may come back in
  either order. *Shared contract (`internal/agentstest.RepoConformance`);
  ordering per backend.*

#### What must be one step

- **A write and the record that it happened.** Two commits mean a failure to
  record reports an error for data that IS stored, and a caller that retries
  stores it twice. Where a backend genuinely cannot — two files — the record is
  best-effort and its failure is **not** reported: what is lost by staying quiet
  is ordering, what is lost by reporting is data.
- **Reading the append point and writing against it.** Two writers that both
  read the same tip mint the same numbers and silently fork the branch — and a
  transaction alone is not one step, because under read committed both still
  read the old tip. A lock over read-and-write (file, memory), a transaction
  owning the pool's single connection (SQLite), or a transaction-scoped
  advisory lock (PostgreSQL): the mechanism is the backend's, the obligation is
  not. The unique constraints above are the backstop for POSITIONS — a race
  that slips through is a failed write, never two rows answering to one seq or
  id. The fork itself only the serialization prevents: two children of one tip
  with distinct numbers violate no constraint a database can see.
- **Reading what is being removed, then removing it.** A record that cannot be
  decoded is still the only copy of what it holds.
- **Claiming and acting.** Checking that a session is still the one you mean and
  then deleting it by name is two steps; between them the name can belong to
  someone else. The check must be part of the write.
- **Selecting a row to remove and removing it.** The delete decides who gets it;
  a caller whose delete affects nothing lost the race and retries.
- **Writing and proving the destination still exists.** A handle held across
  its session's deletion must REFUSE the write (`session.ErrNotFound`), inside
  the same step as the write: a quietly "isolated" write mints entries nothing
  references — invisible to every listing, unreachable by delete, orphaned
  storage by construction. Deletion itself honors the same serialization as
  writes (the repo lock, the write transaction), or it races them into
  recreating what it just removed — and where the proof is a row, that row is
  deleted FIRST, so a concurrent write either blocks on it and then fails or
  has already committed and is deleted with the rest.
  *Shared contract; mechanism per backend.*

#### Absence

- Only "there is no such thing" is absence. Every other failure reaches the
  caller. *Shared.*

### 2.5f Compaction

Compaction is a **run-level** concern. Deciding what to drop needs the model,
the usage numbers and the context window; all three belong to the run, so the
configuration does too (`RunOptions.Compaction`).

- **Nothing is deleted.** A strategy marks groups excluded and may leave a
  folded replacement; the log stays whole and the model's context is a
  projection of it. That is what keeps a compacted session forkable, readable
  concurrently, and inspectable after the fact.
- The run consults its `Compactor` at three points, all three by default:

  | Point | When |
  |---|---|
  | `CompactBeforeRun` | after reading the session, before the first model call |
  | `CompactAtSavePoint` | at each turn boundary, after the turn is persisted |
  | `CompactAfterRun` | once the final output is persisted |

- **A save-point pass rebuilds the context from the log**, rather than editing
  the items in flight. The log is the truth; recomputing a projection is cheap
  and cannot fall out of step with what was stored, whereas splicing a folded
  summary back into a `[]RunItem` has no faithful representation.
- **Compaction never fails a run.** The context it was shrinking is still
  valid, so a failed pass is recorded on the `compaction` span and the run
  continues with the entries it had.
- **"Did the pass change anything" compares whole entries**, via
  `session.Entry.Equal`, not the count and not a chosen subset of the fields.
  Same count with different content is a legal pass — one summary standing in
  for one entry — and so is same ids with a rewritten payload. Calling either a
  no-op discards it silently: the save point does not rebuild, and the
  after-run point writes no checkpoint.
- **An incremental index resumes only on an exact prefix**, compared the same
  way. Entry ids are unique within a session, not globally, so a `Compactor`
  reused across sessions would otherwise hand one conversation's history to
  another. Token usage counts as part of the comparison, because the size
  estimate a strategy budgets against is read off the entries.
- **Local compaction and server-held history do not interact**, because
  `UsePreviousResponseID` / `ConversationID` already refuse a local `Session` —
  there are no local entries for a compactor to see.
- A **self-compacting storage** (`CompactionAware`, e.g. the server-side
  compact API) takes the `CompactAfterRun` point instead; the two never both
  run on one session.

**A checkpoint is appended, never a rewrite — and it copies nothing.**
`CompactAfterRun` records the pass as an `EntryKindCompaction` entry whose
payload names the entries it folded (`ExcludedIDs`) and carries only what
exists nowhere else: the summary text, and a `CompactionFold` per folded group
whose stand-in renders in the group's place (anchored `Before` the first
surviving entry after it). The entries the pass kept are read from the session
itself — never from a copy inside the checkpoint, which would fall out of
step with any later change to the entry it duplicates. The folded entries stay
in the session untouched, so a reader can offer to expand them and a fork from
before the checkpoint still finds its full history. `ContextEntries` leaves folded entries out and
`ProjectEntries` renders each live checkpoint's summary up front, so the next
run reads the shorter context without recomputing the pass.

Writing a checkpoint is an optional capability (`CompactionCheckpointer`): a
compactor that only reshapes the context in memory is useful and has nothing
durable to say.

**A checkpoint is bound to its pass.** `Checkpoint(seen)` names the
entries the caller's own `Compact` saw, and a compactor whose state no longer
describes them — one shared across concurrent runs, re-aimed at another
session between the pass and the checkpoint — reports nothing rather than
recording the other conversation's exclusions (and content) here. A lost
checkpoint costs one recomputed pass; a stolen one is a cross-session leak.

The one path that still rewrites is `openai.CompactionSession`, because the
server's compact API returns a replacement rather than a decision.

**A rewrite built from the response chain never deletes what that chain never
saw.** The last response holds everything that stood in front of the model when
it answered — its own output, and every tool output, handoff acknowledgement and
steer before it. Those are on the chain, and a summary that folds them away read
them first. A log outgrows that chain in four ways, and the runner reports all
four through the single flag `CompactionArgs.OffChainItems`:

- **Position.** What came AFTER the last model-produced item: a terminating
  tool's output, an error handler's fallback message, input injected past the
  last model call. Decided by position and not by provenance, since a steer
  taken after the final output is external and yet reached no model call.
- **A truncated read.** `Conversation.Settings.Limit` sends the model only the
  newest entries, so what the window cut off is stored and on no request — at
  the FRONT of the log, where position cannot see it. Reported only when the
  prepare-time read came back FULL, which is what says the window actually
  truncated; a log exactly the window's size reads full too, so it errs toward
  reporting.
- **A handoff input filter.** What it dropped stays in the log and reaches no
  later model call, mid-log where position cannot see it either. Reported
  whenever a filter RAN, without inspecting its output: telling an identity
  filter apart means comparing CONTENT, since one that redacts in place leaves
  the length untouched, and a comparison that got it wrong deletes the original
  unread.
- **A projector that withholds an item.** `Conversation.Projectors` decides what
  becomes model input; one returning nothing for an `item` entry leaves it
  stored and on no request, anywhere in the log. Only item entries can be lost
  this way — a rewrite carries every other kind over verbatim, and item entries
  are exactly what the summary replaces. Measured per entry, not per config: a
  projector that REWRITES an item is not withholding it, since the model read
  something in its place and the summary stands for it, and reporting merely
  that a projector is installed would never clear.

The last three are facts about the run's past that nothing later undoes, so they
ride across an interrupt/resume on `RunState.OffChainHistory`. A resumed run
re-reads no history and re-runs no filter; answering from its own options
instead would be silently false whenever the caller did not repeat
`Conversation.Settings`, which is the one direction this flag must never fail
in. Position is the opposite case — it clears between runs — so it is
recomputed every time and never carried.

`openai.CompactionSession` answers the flag by compacting from the stored items
instead of `previous_response_id`: the same conversation, minus the deletion. A
caller who PINNED `CompactionModePreviousResponseID` gets the pass skipped and
`abandoned: off_chain_items` on the span instead, because the mode is the one
thing they configured. For position that skip is transient — the next run starts
clean. Past a truncating window it is NOT: a window does not clear, so the pass
is abandoned every run while the log grows. Pinning the chain mode and
configuring a read window is a conflict only the caller can resolve, by dropping
one of the two — which is why the window half is measured rather than assumed,
so a log that never reached its window is never mistaken for that conflict.
**The runner does not decide this by skipping the pass**: a runner-side skip
would take the decision away from a storage with no chain to be wrong about —
an agent that always finishes through a terminating tool would never compact.

**That rewrite is guarded by the sequence number it read.** Reading the history
and writing the replacement are separated by a network round trip, and an entry
appended inside that window is in neither — an unconditional swap deletes it
silently, with no copy left anywhere. So the swap goes through
`GuardedReplacer`: the store compares its highest sequence number back and
writes only while it still matches, comparison and write in ONE step, taken
under whatever already serializes that store's appends. The number compared is
the highest the store HOLDS, not the highest it ever issued — a session
emptied outside the SDK (a workbench deletion) would otherwise refuse every
replace forever — and zero for a log read empty. **A pass that loses the
comparison is abandoned**, not retried and not merged: nothing is written, the
reason is recorded on the `compaction` span as `abandoned`, and the next pass
starts from the history as it then stands, since compaction is housekeeping
and one skipped pass costs size alone. A store without the capability keeps
the unguarded swap: refusing to compact for it would take the feature away
from every third-party store rather than from the race.

**The rewrite keeps the ids of the entries it carries over.** An update entry
names its target by id, so re-minting on the way through leaves it pointing at
an entry no longer there, and a fold that finds no target is dropped in silence
— the late display it carried (a background task's card) lost for good. Those
entries are numbered afresh regardless (§2.5e2).

### 2.5g Context overflow

Compaction predicts; overflow recovery reacts. A prediction is an estimate — a
token count the SDK guessed, against a window the provider never states exactly
— so it will sometimes be wrong, and the failure it misses is one the run
cannot otherwise survive.

- `ExecOptions.Overflow.MaxRetries` enables "compact, then try this turn
  again". **Zero by default**: an overflow is reported rather than silently
  shrinking the conversation.
- **The retry does not spend the turn budget.** The budget counts model calls
  the model made, and an overflow is one it never got.
- **A no-op compaction buys no retry.** Retrying an identical request would fail
  identically, and spending the budget on that is worse than reporting the
  overflow.
- Retries are counted **across the run**, not per turn: a run that overflows
  every turn is not recovering, it is looping.
- **Recovery writes the turn to the session, on the side of the pass the pass
  can survive.** The save point writes first and drains the injection queue
  after, so a steer taken there is in memory only — and recovery rebuilds the
  turn's context from the log, throwing the in-flight items away. The write
  therefore has to happen, or the retry hands the model a conversation the
  caller's words never reached while the next write past their mark still
  counts them delivered ([§2.11b](#211b-run-control)). **When** it happens is
  decided by which recovery applies, and the two want opposite answers. A
  `Compactor` reads the log and returns a projection of it, so the turn has to
  be IN the log before the pass runs. A `CompactionAware` storage may answer
  with a replacement built from its own response chain — nothing produced
  locally is on that chain — so a write made first is a write the pass deletes:
  stored, counted delivered by that very write, then gone, with nothing left in
  flight to roll back. That path writes after the pass and reads the log once
  more, so the turn stands on top of the compacted history. Which path applies
  is decided up front, from whether the storage compacts itself, rather than by
  trying one and falling through.
- The write obeys the usual boundary — a batch ending in a call without its
  output stays held back — and a write that FAILS abandons the recovery: the
  run reports the overflow, the failure is recorded as a `compaction_failed`
  diagnostic so the caller sees WHY it was not recovered from, and the take
  stays in flight for the rollback to re-queue. A run with no recovery
  available at all — no `Compactor` at this point and a storage that does not
  compact itself — writes nothing: there is no pass to prepare for, and the
  write would only spend the rollback the failing run is about to want.
- **A self-compacting storage recovers too.** With no run-level Compactor (or
  with one standing aside because the storage is `CompactionAware`), overflow
  recovery calls the storage's `RunCompaction` with `Force: true` and rebuilds
  the turn's context from the session. Forced, because the storage's own
  trigger normally decides when to compact and an overflow is the one moment
  that question has already been answered — by the provider. The no-op rule
  carries over, sharpened by the abandonment this path can suffer: a forced pass
  buys a retry only if the context came back **weighing strictly less** — the
  summed byte length of the entries' stored bodies, over the same windowed read
  the model is handed. Neither the entry count nor "did anything change" can
  decide it. The read is windowed (`Conversation.Settings.Limit`), and a
  saturated window hides growth perfectly: a storage that abandons its
  replacement because something was appended mid-pass leaves one extra entry
  behind, which pushes the oldest out of the window and comes back the same
  LENGTH — while that same append is exactly what makes the history "changed".
  Weight sees both, and still allows the case a count was there to allow: the
  same number of entries with genuinely shorter content, one summary standing in
  for one entry, is a real compaction. An unchanged history weighs what it
  weighed, so demanding strictly less rules the no-op out on its own. Bytes are
  a proxy for tokens, and deliberately a conservative one: a pass whose result
  does not weigh less costs a retry the run would have spent on a request that
  already failed.
- Detection matches the provider's message, because that is all a context
  overflow arrives as — a 400 with prose in it. Treating every 400 as an
  overflow would compact and retry after a malformed request, hiding a bug
  behind a shrinking conversation. The marker list covers both providers'
  shapes (OpenAI's `context_length_exceeded` family; Anthropic's "prompt is
  too long" and `model_context_window_exceeded`).
- A backend may report overflow in a SUCCESS-shaped response: Anthropic's
  `stop_reason: model_context_window_exceeded` means generation hit the
  window mid-response. The adapter surfaces that as an error carrying the
  marker ([§5.10](../explanation/decisions.md#510-non-responses-backends-adapt-at-the-model-boundary)) —
  resending unchanged would stop at the same wall, and compact-and-retry is
  the recovery that actually helps.
- **A truncated response is NOT an overflow** ([§2.7e](#27e-truncated-responses)).
  Its input fit; compacting the input does not raise the output cap that cut it
  off.
- A recovered overflow is recorded as a `context_overflow` diagnostic.

### 2.5h Crash recovery

`session.Recover` repairs a session a killed process left inconsistent.

- The damage is specific: a run killed between issuing a tool call and recording
  its output leaves a `function_call` with no `function_call_output`. The
  Responses API rejects that history outright, so the session is not untidy —
  it is **unloadable**, and every later attempt to continue fails the same way.
- **The repair appends**, like everything else: a synthesized error output is
  added and nothing is rewritten, so the record of what actually happened
  survives.
- The synthesized output **says what happened** and warns against assuming the
  tool succeeded. A blank result would read to the model as "the tool returned
  nothing".
- **An unfinished call is never retried by default.** A process killed between
  the call and its output leaves no way to tell whether the tool ran — the
  email may already have been sent. Only a tool declaring `RetrySafe: true` is
  left dangling for the next run to redo.
- `RecoveryPolicy.RetrySafe` is supplied by the **caller**, because the stored
  history holds a tool NAME and only the caller knows the agent.
  `RetrySafeNames(tools)` builds it.
- It is the counterpart of `RunState`, not a replacement: `RunState` handles a
  run that paused on purpose and knows where it was; this handles a process
  that died and left only what had been written. `safePersistBoundary` keeps a
  dangling call out on every ordinary exit and cannot help when the process is
  killed.

### 2.6 Guardrails

One `Guardrail` type covers every stage. Placement decides scope: guardrails in
`RunOptions` or on an `Agent` apply to the whole run — their tool stages cover
every tool that agent exposes — while guardrails on a `Tool` apply to
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
- The cancellation runs the other way too: **a model call that fails on its
  own cancels the racing guardrails** instead of waiting them out — a slow
  LLM-based guardrail must not hold an already-failed run open. A verdict
  already delivered still wins; the guardrails' own cancellation error does
  not (it is the stop being honored, and reporting it would mask the model's
  error behind `context canceled`).
- A panicking guardrail is recovered into an error that aborts the run — it never
  crashes the process.
- Every consulted guardrail produces a result, allowing decisions included, so
  callers can read each one's diagnostic payload.

**`Replace` semantics:** the decision's `Message` replaces the inspected content.

- `input` — appended as a single user text message replacing the original input.
  For finer rewriting use `ModelOptions.InputFilter`, which edits the exact
  items sent without changing what is saved.
  **A `Blocking` input guardrail's replacement reaches the model on the
  guarded call itself**: the turn's input is rebuilt from it before the call is
  made — a replacement the result reports but the model never saw is a scrubber
  that did not scrub. A racing guardrail's replacement necessarily misses the
  call it raced (the request is in flight when the verdict lands) and applies
  from the next turn on; a guardrail that must rewrite what the model sees sets
  `Blocking`.
  **A replacement that cannot apply fails the run.** Server-managed turns
  (`UsePreviousResponseID`, a server-held conversation) send only deltas and
  never rebuild from the input — the history the replacement would rewrite
  lives on the server. Proceeding would send the original while the result
  claimed otherwise, so the run fails with a `*UserError` instead; use a
  locally-managed session, or `Trip`.
  **Racing never de-streams the call**: in a streamed run the raced model call
  still yields raw events on the consumer's goroutine; a tripwire cancels it
  mid-stream, and events already yielded stand — the run's error says they came
  to nothing.
- `output` — replaces the final output value.
- `tool_input` — the tool does not execute; `Message` becomes its result.
- `tool_output` — replaces the content returned to the model.

Streaming and blocking share one run loop, so they share one guardrail
behavior: concurrent with the model call, with cancellation.

### 2.7 Tools

#### Return values

A tool returns a `ToolResult` ([§2.7b](#27b-tool-results)); plain values are
wrapped. What the model sees, given the result's `Content`:

| The tool returns | The model sees |
|---|---|
| `string` | verbatim |
| `nil` | `""` |
| `ToolOutputContent` (text / image / file) | native multimodal content items |
| anything else | JSON encoded |
| a value that cannot be JSON encoded | `fmt.Sprintf("%v")` — degraded, never dropped |

An empty result with no error is a **success with no output**, not a failure.

#### Errors

- A tool returning an error goes through `FailureErrorFunction`, which turns it
  into model-readable text fed back to the model. This is the default.
- `FailureErrorFunction == nil` makes tool errors abort the run.
- A tool panic follows the same path, with the stack attached.
- Malformed argument JSON gets dedicated wording that prompts the model to resend
  valid JSON.
- Consecutive all-failed turns abort the run — see
  [§2.7d](#27d-tool-loop-safety-valves).

#### Approval

- `NeedsApproval` / `NeedsApprovalFunc` decide; the function takes precedence.
- If **any** call in a turn needs approval, the whole turn pauses
  (step 4 of [§2.2](#22-ordering-within-a-turn)).
- Approval decisions may be scoped ("this call", "all calls to this tool", …);
  the caller expresses the scope on the `RunState`.

### 2.7b Tool results

A tool returns a `ToolResult`, not a bare value. The distinction it makes is
that some of what a tool knows is **not for the model**:

- `Content` reaches the model. `Details` never does — it lands on the item's
  `Display().Extra`, for the UI and for logs.
- `Title` and `Summary` are display **overrides** on the same never-reaches-
  the-model side: a card heading when the tool name is not it, and a one-line
  account of what happened. Empty means fall back (to the tool name, to the
  existing rendering) — the display contract's "a consumer that ignores the
  hint must still render correctly" is what keeps them optional, and it is why
  neither is ever required.
- `Details` must survive a JSON round-trip. A value that cannot fails the run
  **at the tool call**, while it is still identifiable, not at persistence time.
  An empty map normalizes to nil.
- `Usage` accounts for model calls the tool made itself, so nested spend is
  attributable to the call that caused it rather than only appearing in the run
  total.
- **`Terminate` requires unanimity.** The run stops after a batch only when
  every tool in it asks. One tool wanting to stop while another is still
  working is not a decision the SDK can make for them, and stopping anyway
  would discard the other's result.
- `IsError` marks a failure for renderers; the content still reaches the model,
  which is how a tool that failed usefully lets the model recover. A tool error
  handled by `FailureErrorFunction` sets it automatically.

A tool that returns a plain value (string, struct, `ToolOutputContent`) is
wrapped automatically, so the ordinary tool is unchanged.

- **A multimodal output displays as the wire content list.** `Display().Output`
  of a `ToolOutputContent` / `[]ToolOutputContent` result is the JSON of the
  Responses `function_call_output` content list the model receives —
  `[{"type":"input_text","text":…},{"type":"input_image","image_url":…},
  {"type":"input_file",…}]` — never this package's Go types. It is the one
  shape a renderer can read (an image to show, a file to offer) without knowing
  the SDK, and it is the same on the live stream and in the stored entry.

### 2.7c Tool capabilities are fields

`*Tool` is the only tool type, and everything a tool can do beyond
being called is a **field** on it: `OnInvoke`, `Description`,
`ParamsJSONSchema`, `Strict`, `NeedsApproval` / `NeedsApprovalFunc`,
`Guardrails`, `Timeout`, `Sequential`, `IsEnabled`, `ReadOnly`,
`FailureErrorFunction`, `Deferred`, `RetrySafe`.

- The runner reads them directly. There is no capability lookup, so there is
  nothing to get wrong: a tool's timeout is `tool.Timeout` whether the tool was
  built here or somewhere else.
- **Adapting a tool you did not build is copying the struct.** The schema map
  and validator are built at construction and never mutated, so a copy shares
  them safely:

  ```go
  gated := *tool
  gated.NeedsApproval = true
  ```

- **`Guardrails` is appended to, never assigned**, when adding checks to a tool
  that already declares its own. Replacing the slice would disarm them silently.
- **A hook is captured before being overwritten** when the new one should
  compose rather than replace — `inner := tool.IsEnabled`, then call `inner`
  from the replacement. Nothing enforces this; a caller that overwrites without
  capturing has decided to replace, which is also legal.
- Copying never changes what the model is told unless the copy assigns to
  `Name`, `Description` or `ParamsJSONSchema`.

"Errors abort the run" is the absence of a failure handler:
`FailureErrorFunction = nil`. It is expressible because it is a field — an
absence a wrapper could not have represented.

**Why fields and not an interface with optional side interfaces:** wrappers
hide capabilities — a bare type assertion through a wrapper silently reports
that a tool needing approval needs none. A field cannot hide behind a wrapper.

### 2.7d Tool-loop safety valves

The loop's own failure modes — not the model's ordinary mistakes, but the ones
where an agent keeps going and gets nowhere:

- **Consecutive all-failed turns abort the run.** `ToolLoop.MaxConsecutiveErrorTurns`
  (default 3) counts TURNS in which *every* tool call failed; any success
  clears it, and a turn with no tool calls is neither counted nor cleared —
  the run is talking, not looping. A negative value disables it. Without this,
  a model calling a broken tool spends the whole turn budget rediscovering that
  it is broken, and the caller is billed for it.
- **`ToolLoop.FinalTurnWithoutTools` is opt-in.** With it, an exhausted turn
  budget buys one more model call **with no tools and no handoffs**, so the
  model closes out in prose instead of the run failing. Tool-free is the point:
  offered a tool it would call one, and the budget would be spent again with
  nothing said. It is opt-in because the budget may be a cost ceiling rather
  than a loop guard, and this spends a call it said not to spend.
- **One `Sequential` tool serializes the whole batch.** Per-tool serialization
  would be finer, but a tool that refuses to run beside anything usually means
  it for a resource — a shell session, a working directory — the others touch
  too.

### 2.7e Truncated responses

A response the provider marks `status="incomplete"` with reason
`max_output_tokens` was cut off at the output-token limit.

- **None of its tool calls execute.** Each is answered with an explanation that
  the response was truncated and the call was not run, so the model resends.
- This is a **correctness** rule, not a policy. A truncated response looks
  ordinary — items present, no error — but its tail may be half-formed, and a
  tool call's arguments are exactly the kind of tail that gets cut. Executing
  `{"path": "/ho` as if it were complete is how an agent acts on something
  nobody asked for.
- **Truncation is not failure.** It is fed back to the model rather than
  failing the run, which would throw a turn's work away over a length limit.
  Every other incomplete reason still fails.
- Both model paths report it: `Status` reaches the loop from the blocking call
  and the stream alike, so a response classifies the same way however it
  arrived.
- **None of its tool calls PAUSE, either.** A truncated call never becomes an
  approval interruption: pausing puts a doomed call in front of a human, and an
  approval serialized into a `RunState` and resumed elsewhere would execute
  what the pausing process refuses. The guard runs before the approval
  partition, and `Status`/`IncompleteReason` survive `RunState` serialization
  so a cross-process resume refuses the same calls.

### 2.7f Usage attribution

- **Exactly one entry per response carries that response's `Usage`.** Several
  entries share a response; if each carried it, summing over a session's
  entries would multiply the bill by the number of items a turn produced.
- **It lands on the LAST entry of the response.** That is what makes "how large
  is this conversation now" exact: a reader takes the most recent measured
  input+output as fact and estimates only what follows. On the first entry, the
  rest of that response would be estimated on top of a number that already
  counts it.
- A turn persisted in two batches (an approval pause) attributes on the first
  and clears the flag: a request counted twice is worse than one attributed a
  few entries early.
- A backend that returns **no response id** still has its usage recorded, on
  the batch's last entry.
- **Nested usage is separate.** `session.Entry.NestedUsage` and
  `RunItem.NestedUsage` hold what a tool spent on model calls of its
  own. It is not merged into `Usage`, because the two answer different
  questions: a nested run's tokens were spent on a different conversation, and
  counting them as context would make this one look larger than anything ever
  sent. It IS part of the run total, since the nested run shares the parent's
  usage.
- `RunResult.UsageByResponse()` and `RunResult.NestedUsage()` read it back:
  where the tokens went, and how many were spent off this conversation.

### 2.7h Schema validation

Tool arguments, handoff input and structured outputs are validated against the
**whole** JSON Schema, not a root-level `required` check.

- Nested `required`, nested type mismatches, enums and bounds are enforced. A
  root-only check would let `{"config":{"host":"x"}}` satisfy a schema
  requiring `config.port`, handing the tool a zero value it cannot notice.
- Errors carry a **JSON-pointer path**, which is what a model needs to correct
  its own output.
- Schema `default` values are applied before decoding. A schema that advertises
  a default and a tool that receives a zero value are telling two different
  stories.
- **`additionalProperties: false` is sent to the provider but not enforced
  locally.** An unexpected key is dropped by Go decoding and the tool cannot
  see it, so rejecting the call would turn a harmless extra into a failed turn.
  A misspelled key is still caught, by `required`, which is where it belongs.
- **A schema this SDK cannot compile skips validation** rather than failing.
  It may still be one the provider understands, and refusing to run would turn
  a missing local check into a broken feature.
- Schemas are compiled **once per tool**, not per call. A handoff's schema is
  compiled **once per invocation** instead: the runner validates a per-turn copy
  of the `Handoff` value, so a cache on that value would never be read twice and
  would race the moment two goroutines shared a `Handoff`.
- **An agent tool is a tool.** `AsTool`'s `{"input": string}` and
  `AgentAsTool`'s reflected schema both face this check before the nested run
  starts, `InputBuilder` or not — a builder replaces the *rendering*, not the
  contract the tool advertises. Arguments that fail come back as a
  `*ModelBehaviorError` for the calling model to correct, instead of becoming
  the sub-agent's prompt verbatim.
- **Handoff input carries two rules the schema cannot express.** Arguments must
  be a JSON object — `required` and `properties` say nothing about a scalar
  instance, so a schema omitting `"type"` would otherwise accept `5` as handoff
  input — and absent arguments (`""`, `"null"`) are read as `{}`, rejected with
  "Handoff function expected non-null input, but got None" when the schema
  declares root-level `required` keys. Because neither rule needs a compiled
  schema, both survive a schema this SDK cannot compile, which keeps them and
  skips the rest; a nil schema skips validation entirely. Unlike a tool, a
  rejected handoff input fails the run rather than being fed back to the model.
- Schema `default` values are **not** applied to handoff input, though they are
  to tool arguments: `OnHandoff`, `OnInvoke` and the session all see the model's
  raw argument string, and a value invented during validation would not be in
  it.
- `EnsureStrictJSONSchema` is unaffected: it is the OpenAI strict-mode
  *transformer*, a different job from validation.

### 2.7i Progressive tool disclosure

A tool marked `Deferred: true` is withheld from the model until some
`ToolResult.AddedTools` names it.

- **Marking the tool is the opt-in**, not a run-level flag: the interesting
  question is which tools wait, and a run where everything is deferred could
  never disclose anything.
- **Disclosure is cumulative** for the rest of the run. Withdrawing a tool after
  one use would surprise a model that had just been told it existed.
- **It survives a resume** (`RunState.DisclosedTools`), a serialized
  cross-process one included ([§2.1](#21-the-run-loop): a `RunState`
  round-trips whole). Re-hiding would look, from the model's side, like a tool
  taken away mid-conversation.
- **It does not override `IsEnabled`.** Disclosure opens a door; it does not
  force one.
- **Naming an unknown tool is ignored.** A tool should not be able to fail a run
  by mentioning something.

### 2.7g Tool progress

`ToolContext.Emit` pushes a partial result to a streamed run's consumer as a
`ToolProgressEvent`.

- **Progress is not the answer.** It never reaches the model; the tool's return
  value does. Treating a partial as a result would let a half-finished thought
  become the conversation.
- **Scope is the call.** After the tool returns, `Emit` is ignored. A goroutine
  the tool left running would otherwise keep reporting on a call that is
  already answered, which a consumer cannot distinguish from one still working.
- **No-op on a blocking run.** Nobody is watching, and buffering for a consumer
  that will never read would grow without bound.
- **`emit` serializes with the run loop's own yields.** Several tools stream at
  once while the loop waits on the batch, and an iterator's `yield` is not safe
  for concurrent calls — the mutex is what makes `Emit` possible at all.
- **The consumer's range body runs on the emitting tool's goroutine** for a
  progress event — never concurrently with itself (the mutex above), but not
  on the goroutine that started the range. This is the visible half of the
  serialization trade: marshalling to the consumer's goroutine would need a
  channel and a second consumer loop, which is the producer-goroutine design
  §2.0 removed. Consumers that pin work to the starting goroutine
  (thread-locked UI, goroutine-local state) hand the event off instead —
  stated consumer-side in [streaming.md](../howto/streaming.md).
- A **nested agent-as-tool run is streamed whenever the parent is**, so its work
  shows up as progress without the caller wiring `OnStream`. Only its messages
  are forwarded: relaying the nested raw deltas would bury the parent's stream.
- The sandbox exec tool streams stdout through it, capturing in parallel —
  streaming must not cost the model its output.

### 2.7j Sandbox command policy

`CodeToolConfig.Policy` filters commands **before** the approval gate.

- Before, not after: a person asked to judge forty commands an hour stops
  reading them, so what was never going to be allowed should not reach the
  prompt — and filtering after approval would ask, get a yes, then refuse.
  The veto wraps the tool's own `NeedsApprovalFunc`, so a policy-refused
  command answers "no approval needed" and is refused as text by the tool
  itself.
- `Deny` is checked after `Allow`, so **a deny always wins**.
- A refusal is a tool **result** naming the rule, not an error. Told only "not
  allowed" a model tries variations; told which rule stopped it, it can ask for
  something else.
- The zero value allows everything. **A policy whose patterns do not compile
  refuses everything** — falling open would turn a configuration typo into no
  protection at all, silently, while looking like protection.
- **It is a filter on approval noise, not a security boundary.** A pattern
  matches the TEXT of a command and is blind to shell semantics. Denying
  `rm -rf` steps aside for `rm -fr /`, for `rm  -rf /` with a second space, and
  for `eval $(echo cm0gLXJm | base64 -d)`, which is not the command until bash
  expands it; a rule naming `rm -rf /home/alice` never sees `rm -rf $HOME`.
  Containment is the sandbox backend's job — the policy only keeps the obvious
  out of a person's face.

### 2.7k Persistent shells

`exec_command` optionally reuses a named shell, so `cd`, exported variables and
an activated environment survive between calls.

- **Completion is detected with a sentinel.** There is no other reliable signal
  on a PTY: a prompt is configurable, silence means nothing, and a command that
  prints nothing is indistinguishable from one still running.
- **The token is random and per session.** A fixed one is a token a command can
  print, and `echo __DONE__` would end the read early with a truncated result
  and a garbage exit status.
- **The command line carries the token in two halves.** A PTY echoes its input,
  so a line containing the whole token comes back in the output and cannot be
  told from the real one — the read would stop one command early from then on.
  Only the output ever joins them.
- **The echo is stripped as an exact prefix** of what was written, not by
  pattern: a heuristic would also eat a command that printed its own text back.
- **A timed-out session is closed, not reused.** The command may still be
  running, and its output arriving inside the next one is worse than a shell
  startup.
- **The output awaited for the sentinel is bounded.** A flooding command's
  middle is dropped, keeping the head and the live tail the sentinel must
  land in — a named session cannot buffer unbounded bytes any more than the
  one-shot path's capped streams can, and the result the model sees is
  truncated as usual.
- **A session command that fails still returns the output it produced.** A
  timeout, a dead session or a read error carries the partial output —
  echo-stripped and truncated as usual — alongside the error, matching the
  one-shot path; the tail is often the clue the model needs.
- **Closing the pool preempts a command in flight.** The terminal closes
  first, unblocking the reader, so shutdown never waits out a running
  command's timeout; the interrupted command returns an error, never a
  fabricated exit status.
- Reading happens on a background goroutine, because `Terminal` has no read
  deadline and a blocked `Read` on the calling goroutine cannot be interrupted
  by any timer.
- **Opening a shell happens outside the pool lock**, because on a remote
  daemon it is a network round-trip plus a PTY handshake: under the lock,
  every OTHER name's command queued behind it. Two callers racing the same
  new name both open one; the loser closes its own and takes the winner's,
  so a name still maps to exactly one shell.
- **Closing the pool is final.** It is what keeps opening outside the lock safe:
  a shell landing after `Close` emptied the map would be held by a pool nobody
  closes again — precisely the leaked PTY (on a remote daemon, a live network
  session) that the tool's closer exists to prevent. A named command that arrives
  afterwards fails instead, rather than opening a shell on a sandbox whose owner
  has already let go of it.
- **The named shells belong to the tool, not the run.** Two concurrent runs of
  the same agent share one pool, so both `"build"` sessions are the same shell;
  a host that wants isolation builds the tool per run.
- **The schema advertises `session_id` only when Sessions is enabled.** A tool
  built without Sessions must not offer a field it would silently ignore — and
  strict mode makes every property required, so the model would be forced to
  spell "no session" on every call of a tool that has none. A `session_id`
  sent anyway (non-strict backend) still decodes and is ignored under the
  Sessions gate.

### 2.7l Sandbox tool argument decoding

`exec_command` decodes its own arguments — it is a hand-built tool, not a
`NewTool` wrapper, so nothing upstream of `OnInvoke` catches a malformed call.
Three rules keep one from costing more than the call:

- **Malformed arguments are refused as text, not returned as an error.**
  CodeTool sets no `FailureErrorFunction`, so an error return aborts the whole
  run (§2.7) — the wrong blast radius for the model's own spelling slip, which
  it can correct on the next call. The refusal is an `IsError` result carrying
  the decode error, the same shape as a policy veto. The error return stays
  reserved for sandbox *infrastructure* failure (a dead daemon, a broken
  connection), where feeding text back would have the model retry against a
  sandbox that is gone.
- **The optional string arguments (`workdir`, `session_id`) accept only the
  zero-value sentinels** `null`, `0` and `false`, each decoding to `""`. A
  backend that does not enforce strict schemas (Anthropic, ChatGPT) lets the
  model fill an unused required field with a zero value — `session_id: 0` for
  "no session" — and strict mode is what made the field required in the first
  place. Every other non-string scalar (`true`, `42`, `3.14`) refuses: its
  intent is unknown, and keeping the literal text would run `cd '42'` or open
  a shell named `"3.14"` — a sentinel misread as a value. The refusal feeds
  back as text (previous rule), so the model corrects it on the next call; a
  session genuinely named "0" is still expressible as the string `"0"`. The
  schema still advertises plain `string`. `cmd` stays an ordinary string —
  an empty command is not "unused", it is wrong.
- The approval gate treats undecodable arguments like a policy veto (§2.7j): a
  call `OnInvoke` will refuse as text never reaches a human.

### 2.7m A sandbox reports its own timeout, never the caller's ending

`ExecResult.TimedOut` means one thing: the process was killed for exceeding
`ExecRequest.Timeout`. Every backend derives its per-command deadline from the
caller's context, and a derived context inherits the parent's error — so a
`DeadlineExceeded` read off the derived one may be the CALLER's deadline
arriving, not the command's. The caller's ending is therefore checked first,
and a context that ended for the caller's own reason — cancelled, or a deadline
the caller set — is returned as that error, with no result.

The two backends answer the same way, which is the point: a tool that reads
`TimedOut` to tell the model "that command took too long" must not say it about
a run the human just cancelled.

A timed-out result carries whatever output was collected before the kill; a
failure reading the tail of the output (a broken log stream after the
deadline) costs output, never the `TimedOut` verdict.

### 2.7n A sandbox's environment is part of its container identity

`Options.Env` (docker) sets variables on the CONTAINER, so `exec_command`, a
persistent shell and a terminal opened into it all read the same values —
a person debugging an environment problem sees what the agent sees.
`ExecRequest.Env` overrides an entry for one call only.

The environment therefore joins the adoption fingerprint: a persistent
container created under a different one is **replaced**, not adopted, exactly
as for the image or the resource limits ([decisions §5.19](../explanation/decisions.md)).
Files under the mounted `/workspace` survive that replacement; anything
installed into the container's own filesystem does not.

**A sandbox with no environment set hashes as though the option did not
exist.** An empty map and an absent one are the same container, and their
fingerprint is frozen: changing it would make every already-running container
read as stale and replace the entire fleet.

### 2.7o A docker sandbox runs as the image's user and joins no network

`docker.Options.User` empty means **the image's own user** — for most images
root, which is what lets a container install a package into itself. The
backend applies no user of its own. A caller that wants a narrower one names
it (`"65534:65534"`, `"1000:1000"`); there is no separate flag for "no,
really, empty" ([decisions §5.33](../explanation/decisions.md)).

`docker.Options.Network` empty means **no network at all** (`--network none`).
Any other value is the docker network to join — `"bridge"` for ordinary
networking, a user-defined network's name to put the container where other
containers, and the process that created it, can reach it.

Both are part of the adoption fingerprint (§2.7n): changing either replaces a
persistent container rather than adopting it.

**No network is the default across backends.** `sandbox/e2b` sends
`allow_internet_access` on every create — `false` unless the caller opts in —
rather than inheriting the service's internet-on default, so an un-opted-in
E2B sandbox has no outbound network, the same as an empty-`Network` docker one
([decisions §5.37](../explanation/decisions.md)).

### 2.7p Stop keeps the filesystem and promises nothing else

`Lifecycle.Stop` guarantees exactly one thing: **the working tree survives**.
Whether processes do is the backend's business — docker's stop kills them, a
backend that snapshots memory may bring them back — so nothing may be written
that depends on a process outliving a Stop, and no caller may report one to a
user as "your server is still running".

`Start` after a `Stop` is therefore a resume of the FILES, not of the work.
`Status` reports `absent` (nothing provisioned), `stopped` or `running`; all
three say nothing about the storage, which outlives every one of them.

A backend that cannot control its compute does not implement `Lifecycle` at
all — `LocalSandbox` is one — and callers discover that by type assertion
rather than by a method that returns "not supported".

**A by-name lifecycle call never touches a container it did not create.**
`docker`'s `Stop` and `Status` act on a fixed container name, so — like the
admin `StopManaged`/`RemoveManaged` calls — they verify the ownership
fingerprint before acting: a foreign container that happens to hold the name is
an error, never stopped, and never reported as this sandbox's state. `Stop`
then acts on the resolved id, not the name, so a remove-and-recreate racing the
check cannot redirect it.

### 2.7q A sandbox makes its working directory

A backend creates its working directory on a sandbox it provisioned, rather
than requiring the image to ship one. `docker` gets this from the daemon,
which makes `WorkingDir` when the image lacks it; `sandbox/e2b` does it
itself, once, on the sandbox it created — a stock template has no
`/workspace` and every command would fail with `cwd '/workspace' does not
exist`.

It is done on the FIRST use, not on every one: a resumed sandbox already has
the directory, and a caller that removed it meant to.

### 2.7r A published port is bound to the daemon's loopback, and reaches only 0.0.0.0

`docker.Options.Ports` publishes each container port to **127.0.0.1 on the
daemon's host, on a port the daemon picks** (`HostPort: "0"`). Loopback rather
than every interface: a member's dev server is not something the machine
should serve to its network. Ephemeral rather than fixed: two projects
publishing 3000 must not collide, and nobody has to keep an allocation table.

`URLForPort` and `DialPort` return the PUBLISHED address for a published port,
and the container's own address on its docker network for any other. The
difference matters on a remote daemon: the published address is on the
daemon's loopback, which the SSH transport lands in, while a container address
needs that network to be routable from the daemon's host.

**A published port reaches only what listens on the container's network
interface.** Docker forwards to the container's address, so a server bound to
`127.0.0.1` inside the container is unreachable through a published port,
however correctly it runs. That is docker's model, not a workbench choice, and
the caller is told so rather than left to find out.

Ports are part of the adoption fingerprint (§2.7n): changing the list replaces
a persistent container rather than adopting it, because publishing is decided
once, at create. An ephemeral one-shot container publishes nothing — it has no
service to serve between commands — so only the persistent container binds
ports.

### 2.7s apply_patch locates hunks by whole lines

An Update hunk's context matches **whole lines only**: a match begins at the
start of the file or of a line and ends at the end of the file or of a line,
so `x = 1` can never bind to the tail of `max = 10` and silently edit the
wrong line. The first line-anchored occurrence at or after the previous
hunk's end is the one edited. The `@@` anchor — one context line — binds the
same way, with one tolerance: when no line matches it exactly, the first line
whose space-trimmed text equals the trimmed anchor is taken, so an anchor
written without the file's indentation still lands. `*** Move to:` naming the
section's own path is a plain update, not a duplicate-section conflict.

### 2.8 Nested agent-as-tool attribution

| Aspect | Attribution |
|---|---|
| **Usage** | Folded into the parent run's `Usage` |
| **Trace** | Nested spans join the parent trace, parented by the function span that triggered them |
| **Logging** | The parent's `LogConfig` is inherited, like the tracer — the nested run must not be the silent part of the workflow. Records carry the agent name, so parent and nested lines stay tellable apart |
| **Session** | **Not** shared with the parent; give the nested run state of its own (or the parent's `Session`, explicitly) via `AgentToolConfig.ModifyRunOptions` |
| **Interruptions** | Propagate upward as the parent's own; nested `RunState` is cached on the parent `RunState` keyed by call id |
| **Guardrail results** | The nested run runs its own guardrails; results stay on the nested result and are **not** merged into the parent `RunResult` |

An agent used as both a handoff target and an `AsTool` target follows whichever
path invoked it — handoff shares the run (and its session), agent-as-tool starts
a nested run (with its own session unless configured otherwise). The two paths do
not interact.

### 2.9 Budgets 🚧

`MaxTurns` is the one budget dimension implemented — it counts model calls;
its counting rule across handoffs lives in [§2.4](#24-handoffs). 🚧 Token
and deadline dimensions are unimplemented, and their semantics (what counts,
how they compose) are undecided.

### 2.10 Errors and recovery

- Every SDK error carries a stable `ErrorCode`, read with `CodeOf(err)`.
  `CodeOf` unwraps `%w` chains, so a code survives the run loop's own wrapping
  and a transport can read it off whatever `Run` returned.
- **The code is derived from the error's type — there is exactly one
  classification.** The typed errors carry their data fields and nothing
  else; `CodeOf` maps type → code, so an error built as a struct literal
  classifies identically to a constructed one, and a mismatch between a code
  field and a type cannot exist because there is no code field.
- **A run that fails after its loop started returns a `*RunError`** wrapping
  the cause and carrying the partial progress as a `*RunResult` (nil
  `FinalOutput`): input, generated items, raw responses, usage, guardrail
  results, diagnostics. One shape for finished and failed runs — a failed run
  is a run without an answer, not a different kind of object. It wraps
  UNCONDITIONALLY: a plain error from a hook or a session write carries the
  progress too — wrapping keyed to the cause's type would silently drop it
  for any cause that is not an SDK-typed error. Errors from before the
  loop (bad options, unresolvable model) are returned bare — there is no
  progress to report.
- `Classify(code, err)` tags an error **without hiding it**: `errors.Is` and
  `errors.As` still reach the original. It is how packages outside the run
  loop (`sandbox`, `mcp`, custom tools) contribute a code.
- **The innermost classification wins.** `Classify` returns an
  already-coded error unchanged, so a boundary cannot overwrite a more specific
  reason with its own generic one.
- The code set is **open**. A consumer that does not recognize a code falls
  back to generic handling; this is what lets the SDK add one without a
  coordinated release downstream.
- Errors that a tool returns as *text* (the sandbox file and patch tools) are
  model-facing results, not failures, and carry no code.
- Submodule errors (`sandbox`, `sessions`, `mcp`, `skills`) are classified **at
  the module boundary**, not deep inside.
- Recoverable failures are handled by error handlers (max turns, model refusal,
  invalid final output), **in the loop, not as middleware** — see
  [§2.12](#212-middleware).
- A fallback message synthesized by a recovery handler is tagged
  `Source{Type: SourceErrorHandler}`.

#### How the safety valves compose

`ExecOptions` stacks several independent protections — `MaxTurns`, `ToolLoop`,
`Overflow`, `ErrorHandlers`, `ShouldStopAfterTurn` / `PrepareNextTurn` — and
their interactions are pinned, not emergent:

- **Every recovered final output is still a final output.** A fallback from any
  `ErrorHandlers` handler, and the output derived by `ShouldStopAfterTurn`,
  finish through the same tail as a model-produced answer (`finishRun`):
  agent-end hook, then **output guardrails**, then persistence. A guardrail's
  Replace rewrites a fallback like any other output; a tripwire fails the
  recovery. There is no side door to "finished" that skips the checks.
- Overflow retries spend no turn budget and, having produced no tool results,
  move the tool-loop valve neither way — [§2.5g](#25g-context-overflow).
- Only tool-calling turns count toward `MaxConsecutiveErrorTurns` —
  [§2.7d](#27d-tool-loop-safety-valves).
- **`ToolLoopError` has no handler.** `ErrorHandlers` covers max turns, model
  refusal and invalid final output; a tripped tool-loop valve is always fatal —
  it exists to stop a run that is demonstrably not progressing, and a fallback
  answer synthesized from that state would report the loop as success.
- **An empty final turn retries before it fails.** With no
  `InvalidFinalOutput` handler (or one that declines), a structured-output
  turn that produced no text at all calls the model again rather than failing
  the run; the handler, when set, is consulted first.
- **Queued input outranks the finish.** A `Steer` that missed the save point,
  or a `FollowUp`, continues a run that had produced its final output — the
  continuation happens INSTEAD of `finishRun`, and the turn budget keeps
  counting across it. `MaxTurns` still bounds the continued run, and its
  handler can still recover the overrun.
- `ShouldStopAfterTurn` is asked at turn boundaries, before `PrepareNextTurn`
  — [§2.3a](#23a-the-save-point)'s step order, [§2.3c](#23c-stopping-early).

---

### 2.11 Event fan-out

One producer's events reach many independent consumers through `Fanout[T]`.

- **Publishing never blocks on a consumer.** A subscriber that cannot keep up
  loses items rather than stalling the producer or its peers.
- **A dropped item is always reported.** The next delivery on that subscriber's
  stream is preceded by a `*GapError` naming the range it lost. Silent loss is
  not an option the API offers: a consumer cannot distinguish a timeline missing
  content from one that never had it.
- **Including a cursor from outside the reachable range.** Subscribing below
  the replay window reports the evicted range as a gap running forward from
  the cursor. Subscribing AHEAD of the head — a cursor issued by a previous
  life of the stream, before a restart renumbered it — is a **timeline
  reset**: the gap reports `LastGood` 0 with the stale cursor as its `Dropped`
  count and the timeline's next sequence as `Next`, so the documented recovery
  (resubscribe from `LastGood`) replays the new timeline from its start, the
  gap's own sequence never runs backwards past the deliveries that follow it,
  and it does not read as `AtEnd` — which would tell a consumer to stop
  reading a run that is still going. A fresh timeline must never read as the
  old one's continuation. It is delivered **immediately on subscribe**, not on
  the next publish: the stream a stale cursor lands on has often already
  ended, and a gap waiting for a delivery that never comes leaves the consumer
  in exactly the silence it exists to break.
- **Including when there is no next delivery.** A producer that finishes while a
  subscriber is still behind leaves drops with nothing to ride out on. Those are
  reported as the stream ends, with `GapError.AtEnd` true, `Next` zero and a
  zero-value item — the one case where the item beside a gap carries nothing.
  A consumer resyncs from `LastGood`. Cancelling gets no such gap: the consumer
  chose to stop reading and knows it.
- **Sequence numbers are monotonic and assigned atomically with delivery**, so a
  subscriber never observes a higher number before a lower one — including when
  several goroutines publish concurrently.
- **A subscriber's replay backlog precedes anything published after it
  attached.** Registration and backlog delivery are one atomic step.
- **The zero item beside an `AtEnd` gap is not an event.** A consumer that
  forwards items onward must skip it; forwarding a zero value hands whatever is
  downstream something it has no reason to expect — a nil pointer, for a stream
  of pointers.
- `Close` means "nothing more will be published", not "discard what you have":
  already-buffered items are still delivered. **A publish already accepted is
  one of them**: `Close` waits for it rather than ending the streams first,
  which would lose an item that has a sequence number and sits in replay with
  no gap to report it.

Rejected alternatives, both worse: dropping silently (corrupts the consumer's
view undetectably) and disconnecting the slow subscriber (turns a recoverable
hiccup into a visible failure).

Fan-out is a **requirement, not an optimization**, and that was measured rather
than assumed. A slow consumer couples to the producer under `iter.Seq2`
(13.1× the ideal wall clock) — but it also couples under a buffered channel,
just later: with `chan(64)` the producer still finished at 992 ms against a
100 ms ideal, once the buffer filled. Neither stream shape isolates a slow
consumer on its own, so per-subscriber buffering is needed either way.

---

### 2.11b Run control

`Run` returns a `RunControl` alongside the stream. It is safe to use from
another goroutine, including before ranging begins.

RunControl is stop + injection + pending, nothing more — no introspection
surface: a host renders progress from the stream's own events, which carry
strictly more information. Beyond `StopAfterTurn`, it has three **injection
methods** feeding one arrival-ordered queue; the two consumption points filter
by kind, and only two kinds may extend a run that was ending:

| | Consumed at | Extends a finishing run |
|---|---|---|
| `Steer` | the save point, or the final output | yes — it is "change course" |
| `NextTurn` | the save point only | no — it rides along with a turn the run was taking anyway |
| `FollowUp` | the final output | yes — the exchange lands, then the next one starts |

- **One queue, arrival order.** Injections reach the model in the order they
  were made, across kinds — two messages from the same caller can invert
  meaning if replayed out of order ("do X", then "actually, don't"). The
  kinds are consumption filters, not separate queues.
- **Delivery is transactional.** Taking input moves it in flight; it counts
  as delivered once it lands in a persisted turn, in a serialized
  `RunState`'s item log, or — for a run with no session — in an attempt that
  completed. A failed or abandoned attempt rolls its take back into the
  queue at its arrival position, so a retrying middleware's next attempt
  delivers exactly what the failed one never made durable: nothing lost,
  nothing doubled. A commit fires only against a home that actually holds the
  take: a session write that persisted **past** it (never one that merely
  preceded it), or — at an interruption — a persist that succeeded, after
  which the RunState's item log is the durable home.
- **`RunState.PendingInput` seeds a resumed control once, before `ResumeRun`
  returns it** — not lazily when ranging begins. The control is legal to use
  before ranging, and a lazily-seeded backlog would sequence AFTER input
  enqueued in that window, delivering "new, then old" when the old input was
  said first. The transaction, not reseeding, is what makes input survive
  retries.
- **A follow-up continues the same run**, rather than starting a new one, so
  the trace, the usage total and the session stay one thing.
- **Injected input becomes a run item** with `Source{Type: SourceUser}`. That
  is what makes every downstream path — the next turn's model input, the
  server-side delta cursor, the session write — treat it exactly like the input
  the run started with, instead of each one having to learn about a separate
  pending-input list. It reaches the stream under its own event name,
  `injected_input_created`: `"unknown"` is reserved for `ItemUnknown` — a wire
  type this build does not model — so a consumer that matches on the name can
  tell the two apart.
- **Nothing is silently dropped.** `Pending()` reports what a run did not
  consume, which is how a caller learns a `NextTurn` arrived too late.
- **Queued input survives an interruption**: `RunState.PendingInput` carries
  it — across serialization too ([§2.1](#21-the-run-loop)) — so a steer sent
  while a human was deciding on an approval is delivered on resume. That is
  precisely when someone is looking at the run and saying something about it.
  The wire shape stays the three lists, which does not record cross-kind
  arrival order — an accepted loss at the pause boundary, not worth a schema
  bump.

### 2.11e Span coverage

- Typed spans cover: agent, generation, function, handoff, guardrail,
  compaction, model retry, MCP, sandbox.
- **A retry span is a zero-duration marker**, not a wrapper. By the time an
  attempt is known to have failed it has already happened; recording *that* it
  happened is the point, since a generation span slow from three retries and one
  slow from a slow model look identical otherwise.
- **The current parent span travels on the `context.Context`.** A `Model`
  decorator, an MCP client and a sandbox backend receive a context and nothing
  else belonging to the run, so it is the only channel; a handle threaded
  through signatures would be forwarded by every implementation except the one
  that forgot.
- `StartSpanFrom` returns a **usable no-op handle** without a trace, so an
  instrumented call site never branches and an uninstrumented-context caller
  behaves exactly as before.
- **A finished span belongs to the processor.** `Set` and `SetError` after
  `Finish` are ignored, not applied: from that moment the background exporter
  reads the span, so a late write would be a data race rather than a
  correction. The handover takes the same lock the annotations do — a flag on
  its own is read *before* the write it guards, so an annotation could pass the
  check and land in a map the exporter had already started reading, which the
  runtime answers with a fatal rather than a stale value. Annotating from
  another goroutine is therefore safe and simply drops; writing through the
  exported `Span` field bypasses this and races, exactly as mutating a `Trace`
  after `StartTrace` does.
- **A dropped span is announced through `BatchProcessorOptions.OnDrop`.** The
  processor's queue is bounded, so telemetry is lost under load and after
  `Shutdown`; the SDK does not write to `slog.Default()` on its own
  ([§2.11c](#211c-logging)), which leaves a host-installed callback as the
  only channel that can say so. `Dropped()` remains the cumulative counter.
- The runner installs the generation span as parent for the model call (retries
  nest under it) and the function span for a tool invocation (MCP and sandbox
  work nests under the call that caused it).
- **A span carries the id that joins it to what it produced**: `call_id` on a
  function span, `response_id` on a generation span. Both are recorded whether
  or not sensitive data is — they are ids, not payload — so a consumer holding
  a session entry can find the span that produced it (and a consumer holding a
  span can find the entry) without matching on tool name and arrival order,
  which four identical calls in one turn defeat.
- Sandbox is instrumented at the **tool** layer, the one place every backend
  (local, Docker) is reached through, rather than per backend.

### 2.11d Diagnostics

A `Diagnostic` records trouble a run went through **and survived**.

- The failures worth recording are the ones that do *not* fail the run: three
  retries, a fallback to a slower model, a compaction pass that gave up, a
  recovered tool panic, a tool timeout a `FailureErrorFunction` converted into
  model-visible output. None of them reach an error return, so without this
  they live only in a log nobody kept, and "why was that answer bad" becomes
  unanswerable after the fact.
- They land on `RunResult.Diagnostics` — on a failed run, reached through
  `RunError.Result` — when
  the run does fail (the error is the last straw; the diagnostics are what led
  to it), and on `session.Entry.Diagnostics`.
- **Each is attached to the turn it happened in**, on that batch's last entry,
  not repeated on every turn after.
- **The sink travels on the `context.Context`**, for the same reason the span
  parent does ([§2.11e](#211e-span-coverage)). `RecordDiagnostic` is a no-op
  without a sink, so a decorator used outside a run still works.
- `DiagnosticType` is an **open vocabulary**: an unknown type is displayed
  generically, never rejected.

### 2.11c Logging

- **Off by default.** `LogConfig.Logger` is nil unless a caller sets it; the SDK
  never writes to `slog.Default()` on its own. A library that logs the moment it
  is imported appears uninvited in somebody's production output.
- **Sensitive data is a second, separate opt-in.** Attributes carrying
  conversation content are marked and dropped unless `SensitiveData` is set;
  the record still appears without them. "Log what the SDK is doing" and "log
  what the user said" are different decisions, and only one of them puts a
  conversation into a log aggregator. Outside that opt-in filter, a
  `Sensitive` attribute renders as a redaction marker — its `LogValue` never
  reveals the value, so handing one to your own unfiltered `slog.Logger` is
  safe by default.
- **Every record carries `component`**, so SDK chatter is filterable by origin
  without each call site repeating the attribute.
- **The logger's handler sets the level floor.** Most of what the SDK says is
  `Debug`; hand it a dedicated logger whose handler enables Debug to see it
  without enabling Debug application-wide. (There is no `Level` override
  field: ANDed with the handler's own gate it could only tighten, never
  loosen.)
- Logging and tracing are configured separately, as are their sensitive-data
  switches: exporting spans and writing log lines are different exposures.

### 2.12 Middleware

`RunOptions.Middlewares` wraps a run, **outermost first** — the order they are
read in is the order they see the run.

A middleware wraps a *whole run*: it may edit the input and options, call `next`
zero or more times, and replace or suppress events. That is what it is good at
— retrying, re-running with feedback, resuming from an interruption — and it is
also what bounds it.

**What is not middleware, and why:**

| | Why it stays in the loop |
|---|---|
| Handoffs | Change which agent the state machine is in |
| Guardrails | Race the model call and can cancel it |
| Session persistence | Has a boundary only the loop knows ([§2.5](#25-session-persistence-boundaries)) |
| Tracing | Spans nest with the loop's own structure |
| `ExecOptions.ErrorHandlers` | Needs the run's in-flight items to build `RunErrorData`, and the loop's completion path to persist what it recovers. A middleware sees a terminal error and can reconstruct neither |
| `ModelOptions.InputFilter` | Per **turn**, not per run |

Expressing any of them as middleware would turn an invariant into an implicit
protocol between wrappers.

**A middleware must not swallow the stream.** One that re-enters the run
forwards each attempt's events and holds back only `RunCompletedEvent`, which
is the one event whose meaning it owns — "this attempt finished" versus "the
run finished". A middleware that buffered everything until it was satisfied
would make a long retry look like a hang, which is the opposite of what
streaming is for.

That norm is a three-clause contract, and an author owes all three (stated on
`RunMiddleware`'s godoc, where an author starts):

1. Every event other than `RunCompletedEvent` flows through as it happens.
2. `RunCompletedEvent` appears **exactly once, last**, on a run that ends
   without error — and zero times on one that errors. A re-entering middleware
   therefore holds back each attempt's completion event and emits a single one
   for the attempt it accepts.
3. Once the consumer stops ranging — yield returned false — nothing more is
   yielded, **not even an error**: there is nobody to receive it.

The shipped middleware that re-enter or terminate a run keep the contract
through the package's internal `collect`/`finish` helpers (a pure pass-through
that only observes keeps it by construction); a third-party author implements
the same three clauses directly.

**Order is behavior.** A middleware that resolves something about *one attempt*
(answering an approval pause) must sit inside one that decides whether to make
*another attempt* (an evaluator loop, a retry). Reversed, the outer one judges
a result the inner one had not finished producing.

**A stop the caller asked for is visible on the result** (`RunResult.
StoppedEarly`), wherever the run ends — at the turn boundary that saw the
request, and equally on a run that reached its final output on that same turn.
The flag answers "did the caller stop this", not "where did it stop": the stop
lives on the control for the whole run and is never cleared, so a middleware
that re-runs (`Loop`) cannot tell "the agent finished" from "the human stopped
it" without it, and started every remaining attempt — including for
single-turn agents, which never reach a turn boundary at all.

**A middleware that resumes strips `Middlewares` first.** The chain is already
unwound at that point; resuming with the run's own options would re-enter that
middleware and every one outside it.

**The public `ResumeRun` applies `opts.Middlewares` exactly as `Run` does.** A
caller resuming with the options it ran with gets the wrapping it ran with —
`Loop` still evaluates, `Retry` still retries, `Approval` still resolves
further pauses. (The rule above is what keeps the two from compounding: an
in-chain resume passes stripped options.) The paused state's agent and input
are already decided; a middleware's edits to those fields do not apply on
resume.

**Workflow middlewares (`Plan`, `Todo`)** rewrite the ENTRY agent only —
handoff targets keep their own toolset, the same scoping as every
instruction-injecting middleware. Their invariants:

- **Plan gates by DENYING, not hiding.** While planning, a tool outside the
  read-only set stays in the model's toolset and answers a call with a refusal
  naming `submit_plan` — a normal tool OUTPUT, not an error (an error without
  `FailureErrorFunction` aborts the run, and a phase decision is not a
  failure). Hiding was worse in practice: a model that cannot see a tool it
  expects calls it anyway, and "tool not found" teaches it nothing about the
  phase — it cannot tell a gated tool from one this session never had. MCP
  tools are gated the same way, per turn, since their set is unknown at build
  time.
- **Handoffs, unlike tools, ARE hidden while planning** (`Handoff.IsEnabled` —
  a target's full toolset would otherwise be a side door out of plan mode).
  The asymmetry is deliberate: a model carries priors about tool NAMES and
  reaches for them unprompted, but has none about this agent's handoff
  targets, so hiding one costs no wasted turn. That gate COMPOSES with the
  predicate it wraps rather than shadowing it — the resolver consults only the
  outermost layer, and unlocking must not resurrect a handoff the host itself
  disabled.
- **A FIRST-PARTY tool's `ReadOnly` is trusted; an MCP tool's is not.** A
  direct `*Tool` sets `ReadOnly` about itself and the gate honors it (sandbox
  `read_file`/`list_files` stay usable while planning). But on an MCP tool that
  same flag came from the server's `readOnlyHint` — a claim an OUTSIDE server
  makes about itself, and plan mode's "nothing changes until you approve"
  guarantee cannot rest on an outside claim: a malicious or mistaken server
  could mark a write tool read-only and run it mid-plan. So `planMCP` admits an
  MCP tool ONLY when the CALLER named it in `ReadOnlyTools` (`DefaultReadOnlyTools`
  when nil) — a first-party allowlist — never on the hint alone. Neither path is
  enforced beyond that: nothing checks a tool that claims read-only behaves, so
  the allowlist is a statement of trust in a NAME, which is the caller's to make.
- **The refusal outranks approval.** The runner's approval partition runs
  BEFORE a tool invokes, so a gate that only wrapped `OnInvoke` would pause a
  human over a call the phase then refuses — approving would execute nothing.
  While planning, a gated call therefore needs NO approval: not the tool's own
  predicate, and not the agent-level `ApproveTools` listing, which `Apply`
  translates into per-tool predicates (and clears off the clone) precisely so
  the phase can suppress it. Once executing, the translated predicates answer
  exactly as the tool-then-list order did. A READ-ONLY tool the listing names
  keeps its approval in BOTH phases — the phase never suppresses approval on a
  call it is not refusing. The translation covers MCP tools per listing
  (`planMCP` carries the matcher), including a `"*"` wildcard.
- **The unlock's SCOPE is the host's to decide, and `PlanPhase` is per RUN.**
  `Apply` mints a fresh phase for every run, so a host that wants an approved
  plan to hold across later turns consults its own durable record and calls
  `Unlock` before the run — which is what `OnUnlock` exists to make possible.
  The SDK offers no session-scoped phase: it has no notion of a session.
- **`Plan.Apply` is safe to call unconditionally, so WHETHER this run plans is
  the phase's answer, not the build's.** An already-unlocked phase gates no
  tool, offers no `submit_plan` (`IsEnabled`) and contributes no preamble —
  the planning instructions are emitted per run, only while the phase is
  locked. That is what lets a host decide plan mode outside the agent (per
  session, per request, per person) and still rebuild the same agent for a
  durable resume: building it only for a run that plans would leave the
  rebuild — which happens AFTER the unlock — without the `submit_plan` the
  paused state names.
- **The plan review is an ordinary approval pause.** `submit_plan` is
  approval-gated always; the plan text is the call's arguments. Approving it
  unlocks the toolset and the SAME run continues; rejecting feeds the message
  back and planning continues, write tools still refusing. No second pause
  mechanism exists for hosts to learn.
- **`todo_write` replaces the whole list, atomically.** The model always sends
  every item (simpler to prompt for, impossible to desynchronize); a malformed
  list is refused whole, so `OnUpdate` never observes a half-applied state.
  An empty status defaults to pending. `todo_write` is on
  `DefaultReadOnlyTools`, so stacking Todo with Plan works in either order.
- **The rewrite is exported as `Apply`, for hosts with durable resume.** A
  host that deserializes a paused `RunState` against a rebuilt agent registry
  must rebuild WITH the plan/todo tools, or the approved `submit_plan` fails
  with "tool not found" — so `Plan.Apply` / `Todo.Apply` run the same rewrite
  at agent-build time. `Plan.Apply` also returns the run's `*PlanPhase`;
  `Unlock` starts a rebuilt run in the executing phase, which is how a resume
  after the plan phase ended avoids demanding a second plan.
- **What a durable-resume host persists is the UNLOCK, not the approval —
  and persisting it is the unlock's PRECONDITION.** `PlanPhase.OnUnlock`
  fires once, when the approved `submit_plan` executes; its error fails the
  unlock and the phase stays planning, so a run is never executing ahead of
  its durable record (the failed write surfaces as a submit_plan tool error;
  the model resubmits and the review repeats). Neither weaker signal
  survives scrutiny: the approval ledger records approvals whose execution
  then failed (argument validation, say), and the tool's output text can be
  rewritten by a tool-output guardrail.

---

### 2.13 Background tasks

A task is a sub-agent that outlives the turn that started it. The invariants
below are behavior, not implementation detail — see [tasks.md](../howto/tasks.md).

- **Identity and execution are separate.** `Task.ID` is the durable entity,
  `Task.RunID` one attempt at it. Collapsing them makes a retry inexpressible
  without inventing a second task.
- **Finalization is a compare-and-set**: status and result in one atomic
  transition, only while the task is non-terminal. This is why a task store
  must be transactional, and why no file-backed one is offered — a
  read-modify-write cannot arbitrate between two finalizers.
- **The Manager REPORTS endings; it does not deliver them.** A terminal
  transition it claimed calls `Config.OnFinished`, and a result the model
  pulled in-turn with `task_status` calls `Config.OnResultDelivered`. Deciding
  when a session may be interrupted, and holding the debt until it may, is a
  host policy — a task that finished while its parent was busy, paused, or
  down is the host's problem to time, and the SDK owning a wake-up state
  machine put that policy in the wrong place. **A cancellation is reported as
  DELIVERED, not finished**: the person did it and a turn restating it would
  only repeat them. **The reported `*Task` is the claimed snapshot in hand**,
  built from the finalize's own values rather than a re-read: between the win
  and a read, a retry can move the row past this attempt (non-terminal, the
  failure cleared), and a failed read must not cost the parent the report.
- **What a durable host owes on top of the reports.** `OnFinished` is a call,
  and a crash can fall between the terminal write and it — so a host whose
  delivery must survive crashes writes its own debt row ATOMICALLY with its
  `Store.Finalize` (and `ReleaseRetryClaim`), not from the hook. The same
  transition discipline holds the other way: a host holding an undelivered
  debt drops it inside `RetryClaim`'s transition (the task is no longer
  finished; the next ending owes a fresh one), and its restart sweep writes
  each orphan's debt in the sweep's own transaction. The interface cannot
  carry these guarantees (an in-memory store has no debt to write), so they
  are recorded here instead.
- **A task is the ONE shape of background work, and its work may span several
  runs.** A job of a fixed step sequence, or a loop until a check passes, is a
  task whose runs are chained rather than a second lifecycle beside tasks.
  Everything else — the hidden session, stop, retry, the restart sweep, the
  approval pause, the cap, the wake-up — is then written once.
- **`Config.Continue` is asked when a run of the current attempt completes or
  fails** — only when the outcome NAMES the run (an outcome without a run id
  can only finalize the attempt the row names, never advance it: a duplicate
  delivery would otherwise bind to whichever run is current and advance
  twice) and only while the row is still working on it (a row paused for an
  approval is not moved on: its run is not over). The hook is never asked
  about a cancellation (a person's stop ends the task whatever the host would
  do next) nor about a superseded attempt's outcome (it would lose the
  transition anyway); an error from it ends the task failed with that reason,
  and a next run that fails to launch ends it failed too, reported like any
  ending.
- **A `Continuation` moves the task to its next run through `Store.Advance`**
  — run id and the host's `State` replaced in ONE compare-and-set, only while
  the task is working on the run the hook was asked about (a nil `State`
  keeps the recorded one, as `Finalize` does). Without an `Input` it ends the
  task instead, its final `State` written in the same `Finalize` as the
  ending (`Store.Finalize` carries it), so the record of a job's last run and
  its status are one write. A transition the claim does NOT win is finalized
  on the run that ended, failed — `Finalize`'s own predicate then decides: a
  stop, a sweep or a retry that moved the row wins as before, while a row a
  pause report of that same run put back to `input_required` inside the
  hook's window (an ordering the store contract allows) ends rather than
  strands on a run nobody will resume. `Advance` with the same run id on both
  sides rewrites `State` under the CAS, which is how a launcher records the
  run it is about to start beside no second write.
- **The chain is bounded.** `Config.MaxContinuations` (default 50) is how
  many further runs the hook may chain under one task since the spawn or the
  last retry — a hook still asking at the bound ends the task failed, the
  ceiling on a loop no check ever ends — the same posture as
  `MaxAttemptsPerTask` and `MaxDepth`: every axis a task can grow along has
  one.
- **`Task.Kind` and `Task.State` are the host's vocabulary and record, opaque
  to the SDK** (`Config.DescribeState` is how a host says where a job of its
  kind stands, in one line the task tools show) — which is the layering: the
  SDK owns the durable multi-run job, the host owns what a job of a given
  kind IS (a workflow's definition, its steps, its edges).
- **One cap governs every kind of background work** — a consequence of the
  above: every kind is a task, so `MaxConcurrentPerParent` counts them all,
  and nothing can hide behind a count of its own.
- **One vocabulary for the model: four verbs, whatever the kind.** The
  model-facing surface is `spawn_task`, `task_status`, `task_retry`,
  `task_stop`, and a host with more kinds of background work than a plain
  task does not add a fifth tool: it provides its own spawn tool from the
  public parts (`SpawnTool` / `TaskTools`, `Spawn`, `ModelHasResult`,
  `ToolResult`) with the kind as a parameter, and `DescribeState` makes
  `task_status` answer for that kind. Two tools that both mean "start
  background work", or both mean "look at it", are the tool-choice errors a
  small model makes; the count of concepts is what is kept small, and it is
  one.
- **A task row names sessions by GENERATION, not by id.** A session id names a
  session, not a place (§2.5e2), and a task outlives the turn that spawned it
  by design — so a row matched on the id alone attaches itself to whatever
  session holds that name later: the replacement lists a dead incarnation's
  tasks, is woken for results it never asked for, and the debt is retried at
  every restart forever. A store that persists task rows binds the parent's
  and the child's generation when it writes one, and every by-session read
  compares them against the generation answering to that id NOW. **A
  session.Repo that owns both tables deletes the task TREE with the session**:
  the task rows in both roles, and the hidden sessions the tasks ran in, at
  any depth — the generation makes a surviving row inert, the cascade is what
  stops it surviving, and a hidden session left behind is unreachable forever.
  *Per backend, because only a backend that holds both can answer it; the
  Manager deletes nothing, and a repo without a task table (the in-memory
  repo) leaves the rows and the child sessions to the host — the boundary
  docs/tasks.md "Deleting a session" states.*
- **A notification line is machine-readable, and its fields come from
  untrusted text.** A label and a result are model output; formatting escapes
  the line delimiter AND the field delimiter, because the line pattern's own
  greediness otherwise lets a crafted result re-aim the task id and status on
  the very same line — forging a card for a task the sender does not own.
- **A task carries the configuration snapshotted at spawn** (`Task.Inherit`)
  and the run that spawned it (`Task.ParentRunID`), so a host delivering the
  result later runs the turn as the agent that asked and can record the
  relationship on the run's own durable output (its traces) rather than
  re-deriving it from task rows or notification text, which a fork or a fold
  does not carry.
- **A restart fails what it interrupted and RETURNS the rows.** `FailOrphans`
  answers with the tasks it failed, not a count: each parent still has to be
  told, and only the caller can arrange that. `input_required` is left alone:
  its approval persists.
- **`input_required` is not terminal.** A task waiting on a human is in flight;
  a notification for it would announce something that has not happened.
- **A paused task is claimed before the host is told to stop it** (the finalize
  is the exclusive claim against a concurrent approval); a working one has its
  run cancelled first, or the run's own completion records a success for
  something the user stopped.
- **Rollback of a half-finished spawn uses a detached context.** `Spawn` runs
  inside the parent run, so a parent cancellation racing it would kill the
  cleanup halfway.
- **A depth check that cannot be made refuses**, the same rule a host's wake
  guard follows. Depth is read from the store, and a lookup that fails is not the same
  answer as "this parent is not a task" — that one restarts the count at 1, so
  treating them alike makes one transient query error a way past the limit.
  `MetaFor` reports the failure rather than resolving it to "no".
- Defaults: depth 1 (a task cannot spawn tasks), 6 concurrent tasks per parent,
  300-rune summaries, a 120s bound on `task_status`'s wait, 3 attempts per task.
- **A notification is a user-role entry** the model reads verbatim; a UI renders
  it as a card. Formatting and parsing ship together, so the format is defined
  once.
- **A failed task can be retried in place** — `failed → working`, the only
  transition out of a terminal state, and a compare-and-set like `Finalize`:
  the new run id, the incremented attempt and the cleared summary and result
  land with the status, only while the task is failed and under the attempt
  ceiling. **The ceiling is a store
  predicate**, not only a Manager check, so two processes cannot both claim the
  last attempt. Resuming is sound because the session is: persistence stops at
  a boundary that never leaves a call without its output (§2.5), so the tail of
  a failed attempt is valid model input.
- **A finalizer names the attempt it observed.** Reopening a terminal task
  removes the invariant the rest rested on — "the row is non-terminal" no
  longer says WHICH run a writer looked at — so `Finalize`, `Advance`,
  `ReleaseRetryClaim`, `MarkInputRequired` and `ReclaimWorking` all carry a
  run id and lose when it is not the current one (a host's own durable debt,
  written with the Finalize, is bound the same way). A stop that read the row
  just before a retry would otherwise cancel the new attempt while its run
  kept executing, unkillable, its own outcome discarded for losing the CAS;
  an approval opened on one attempt must not pause or resume the one that
  replaced it. A stop chases **one** retry, since it names the task rather
  than the run.
- **A retry takes a concurrency slot** like a spawn — exempting it would make
  retry the way around the cap — and a launch that fails puts the task back to
  failed. That ending follows the model-path rule below: the `task_retry` tool
  reports the failure in hand (so it counts as delivered), while a retry over a
  host API told only a person and the model still has to hear it.
- **A stop reports what it DID**, not whether it errored, and the four answers
  are not interchangeable. A host asked to stop a run it has never heard of —
  the ordinary state during a launch — has nothing to report but success, and
  reading that as "the run will wind itself up" answers a graceful stop with
  acceptance while the task runs to completion and nobody records anything.
  **`StopAfterTurn` is the only answer that ends the call**: that run is still
  going and will record its own ending. `StopAlreadyFinished` — the run ended
  before the stop arrived, which is ordinary because a host marks a run
  finished before its outcome reaches the task row — claims nothing and sends
  the stop round again. It must not write a cancellation, which would overwrite
  a real completion or a failure along with the retry it had earned; and it
  must not end the call either, because "that run is over" is also what a stop
  hears when a RETRY landed between its read and the call, and standing back
  there would leave the new attempt running with the stop reported as done.
- **An outcome that is late is waited for; one that is lost is replaced.** The
  two are indistinguishable in the moment — both are a dead run under a live
  row — so the stop waits, briefly and boundedly, for the ending to arrive
  before its last pass, and records a cancellation only if none does. Waiting
  is what keeps a real completion; the bound is what keeps a task whose outcome
  never landed (a host whose store refused the write) from being **un-stoppable**:
  reporting it as still working leaves a dead task live in every UI, with a
  timer that never stops and a stop button that changes nothing. A host helps by
  answering `StopAlreadyFinished` only once the run's own recording has had its
  chance — for the server that means waiting on the segment's done gate, which
  closes after the outcome is written — which turns the ordinary race into no
  wait at all and leaves the SDK's bound for the genuinely lost case.
- **Whether a run reported is the Manager's own knowledge, not the row's.** A
  run that finished the instant it started and a run something ended while the
  host could not reach it leave the SAME row: terminal, on that run id.
  Compensation looks at both — the row, and whether `OnRunFinished` has spoken
  about that run — because cancelling on the row alone cancels tasks that
  simply finished quickly, which is the common case when a run fails its
  pre-flight.
- **A result counts as delivered on the MODEL's path only, and only for the
  result the model is actually handed.** When a task ends before the call that
  started it returns, its result is in the tool output the model is about to
  read, so `OnResultDelivered` fires and the host drops what it was going to
  deliver — the rule `task_status` already followed. Two bounds keep that from
  swallowing news instead: a task still reported as running is NOT delivered
  however the row reads by then, because a result that landed after the answer
  was decided is one the model has not seen; and the attempt is checked, since
  a retry in between makes the outstanding delivery a different attempt's. The
  rule covers refusals too: `task_retry` answers every call that has task state
  with that state — success, refusal, or a launch that never started — and
  whatever terminal result the report carries is thereby in the model's hands.
  A person reading the same result over an HTTP response has told the model
  nothing, so a host API must not report delivery at all.
- **Retryability is about the task's own state, and the ceiling is the
  Manager's.** A failed task that has used every attempt looks exactly like one
  that has not, so a caller offering a retry needs the ceiling — `MaxAttempts`
  hands over the parameter, and the caller derives the answer from the status
  and attempt it already tracks, so its offer moves with the state rather than
  lag a round trip behind it. The parameter is the WHOLE api on purpose:
  every consumer (server relay, web UI, the model tools) derives from state
  in hand, and a precomputed per-task boolean would go unread.
  **Capacity is deliberately excluded**: the parent's live-task limit
  can change between an offer being rendered and someone taking it, so a
  precomputed answer would be wrong as often as right — that refusal arrives
  as `ErrTaskLimit` at call time, which explains itself; a retry that loses
  its claim to a concurrent writer is `ErrRetryConflict`, a conflict to retry,
  not a fault.
- **Starting a run is two steps, and a terminator can land between them.** The
  row is claimed (created, or reopened by `RetryClaim`) before the host is told
  to start the run, so a stop arriving inside that window cancels a run the host
  has never heard of: its `Stopper` call reaches nothing, and the launch goes
  ahead regardless. The result is a run executing for a task that is already
  cancelled — unstoppable, and unable even to record its own outcome, since the
  row it would finalize is no longer its own. Both halves are closed: a stop
  tells the host **again** once the ending is unambiguously its own (the run is
  reachable by then), and `Spawn`/`Retry` **re-read the row after launching** —
  if it no longer names their run as its live attempt, they cancel the run they
  just started and report what the task actually is. The second half is what
  covers the terminators that never speak to the host at all: an approval
  reaper, a restart sweep.
- **Every attempt-scoped write names its attempt** — `Finalize`,
  `MarkInputRequired` and `ReclaimWorking` alike. "A non-terminal state can
  only belong to the current attempt" is no argument for an unbound writer,
  because an APPROVAL breaks it: persisted before the pause lands on the task
  row, it can outlive its attempt across a crash, a `FailOrphans` sweep and a
  retry — and an unbound writer acting for it would pause, reclaim or
  (through the expiry reaper) cancel the attempt that replaced its own. All
  four transitions carry the run-id predicate; a stale approval's write is
  a silent no-op, its resolve is refused as stale (and discarded, not retried
  — restored it would refuse forever), and the reaper finalizes against the
  expired approval's OWN run id, never the row's current one.
- **A launch that failed never counts as an attempt.** `Attempt`'s contract is
  the runs the task has *had*, and `RetryClaim` increments it before the host
  is asked to launch — so a launch refusal (shutdown, session deleting) would
  otherwise spend the retry ceiling on runs that never existed, until every
  attempt was gone without a retry executing. `ReleaseRetryClaim` is the undo:
  bound to the claimed run id like `Finalize`, it puts the task back to failed,
  rolls the attempt back down (floored at 1 — the original run always counts)
  and records the launch failure as the task's result, which is reported like
  any other ending. Only the launch path releases; a run that registered and
  then failed is a real attempt and finalizes normally.
- **The restart sweep cannot arbitrate a retry**, so ordering must. `FailOrphans`
  fails every row recorded as working and has no notion of a live run; a retry
  that claimed first would have its fresh run declared dead, its parent told of
  a failure that did not happen, and the real result dropped for losing the
  CAS. The sweep is therefore a separate call from whatever delivers, to run
  **before** the host accepts requests. Two processes sharing one store keep the race —
  the same exposure the concurrency cap already documents.
- **The retry hint in a notification is its own line.** A task line is a record
  consumers parse, and text appended inside one is read as part of that task's
  result — the same reasoning that makes the label and summary escape their
  delimiters.

### 2.14 The SDK reads no environment variable

Everything the SDK acts on is passed in: `RunOptions`, the `Agent`, its
`ModelSettings`, and the provider/backend constructor options. The `agents`
package calls no `os.Getenv`, so two differently-configured runs in one process
are fully determined by what each was handed — nothing ambient decides behavior
behind the caller's back. `rg 'os\.(Getenv|LookupEnv)' agents/` is the guard,
and must stay empty.

Two touchpoints are outside this rule and are not violations of it:

- **A wrapped vendor client library keeps its own env contract.** The OpenAI
  provider delegates to openai-go, which defaults its key from `OPENAI_API_KEY`
  when the caller passes no `option.WithAPIKey`. The `agents` code never reads
  that variable; the caller opts into the default by constructing the provider
  without a key, and overrides it with an explicit option.
- **An OS-integration backend may consult the standard variable of the tool it
  drives.** The docker sandbox reaches an SSH agent through `SSH_AUTH_SOCK`,
  exactly as `ssh` itself does. It is documented on the backend and overridable
  by an explicit dial option; it configures a transport, not run behavior.

The trace sensitive-data toggle (`Observe.IncludeSensitiveData`) is the one
knob this rule reclaimed: nil now means include (§4), decided by the caller, not
by a `OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA` variable — see
[decisions §5.39](../explanation/decisions.md).

---

## 4. Reference behavior you can rely on

Defaults that callers may depend on:

| Setting | Default | Note |
|---|---|---|
| `MaxTurns` | 10 | `MaxTurnsUnlimited` (-1) disables it |
| Strict schemas | on | Chaining `NonStrict()` relaxes both the advertised schema and local validation, atomically — but only on a tool that got built; an argument type strict mode cannot express at all needs `NewToolNonStrict` ([§5.11](../explanation/decisions.md#511-construction-errors-split-by-data-provenance)) |
| Handoff input schemas | strict | `Handoff.NonStrictSchema: true` opts out; the zero value is the strict default |
| Tool errors | fed back to the model | `DefaultToolErrorFunction`; set the field to `nil` to make them fatal |
| Tool concurrency | unlimited | Bound with `MaxToolConcurrency` |
| Input guardrails | concurrent with the model call | `Blocking: true` makes one a gate |
| Session persistence | after each turn | Final turn is written after output guardrails pass |
| `RunResult.Usage` / `RunState.Usage` | detached snapshot | Never the live accumulator; read without synchronization. Mid-run, `RunContext.Usage` is live — read it via `Snapshot()` |

---

## 6. Open questions

### 6.1 What a v1.0.0 promise means while openai-go's major can move

§5.5 makes the Responses wire types the canonical format, and §5.5b accepts that
they therefore appear in nearly every exported signature by way of `InputItem`
and friends. Both decisions stand. What neither records is the consequence for
§5.8: **an `openai-go` v3 → v4 bump is transitively breaking for every
downstream package**, not just for this one. Their function signatures name
those aliased types, so their code stops compiling on the day this module's
`go.mod` moves — a break this SDK causes but does not author.

That is survivable before v1.0.0, where §5.8 already allows breaking minors. It
is the question after: a v1 that promises compatibility is implicitly promising
`openai-go/v3`, since honoring the promise and taking the bump cannot both
happen. Three answers, none chosen yet:

- **Take the bump as agents-go v2.** Honest, and expensive for an ecosystem that
  has to move in lockstep with a dependency's schedule rather than ours.
- **Fork the item types.** Buys independence at exactly the cost §5.5b declined:
  a conversion layer that must chase every Responses API addition forever.
- **Stay on v3 for the life of v1.** Cheapest until a provider feature only the
  new major exposes, at which point the cost arrives all at once.

Whichever is taken, it belongs in §5 before v1.0.0 is tagged, not after.

### 6.2 The `skills` module fails §5.7's own test

The `skills` module's only non-root direct dependency is `gopkg.in/yaml.v3`,
which brings zero transitive requirements — not the heavy dependency §5.7
makes the sole justification for a submodule. Folding `skills` back into the
root module is the consistent move, but it is a breaking module change
(import paths move), so it waits for a breaking window. Open until decided.

When a new case comes up that this document does not answer, add it here with
the options under consideration.

---

## 7. Change rules

1. Any PR that changes behavior described here **must update this document in
   the same change**.
2. A new §6 entry does not need an immediate answer, but the PR that implements
   it must move it out of §6 first.
3. A decision — the reasoning, not the resulting behavior — is recorded in
   [design decisions](../explanation/decisions.md) under a new §5 number.
   Numbers are never reused: code comments cite them as `decisions §5.29`.
4. Upstream changes are tracked in [upstream_watch.md](../explanation/upstream_watch.md) with
   **no obligation to match**.
5. Users migrating from the Python SDK: see
   [migration_from_python.md](../explanation/migration_from_python.md).
