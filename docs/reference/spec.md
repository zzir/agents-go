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
the invariant, and code must satisfy it. The sentence after it bounds it; it is
not a second rule, and it is never a reason — reasons live in
[decisions](../explanation/decisions.md). Section numbers are permanent
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
| [§2.5e](#25e-session-lifecycles) | Session lifecycles | `session.Repo` owns which sessions exist, apart from their contents |
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
| [§2.7n](#27n-a-sandboxs-environment-is-part-of-its-container-identity) | A sandbox's environment is part of its container identity | `Options.Env` reaches every command; a container is adopted only on a matching fingerprint |
| [§2.7o](#27o-a-docker-sandbox-runs-as-the-images-user-and-joins-no-network) | A docker sandbox runs as the image's user and joins no network | Empty `User` = the image's own user; empty `Network` = none |
| [§2.7p](#27p-stop-keeps-the-filesystem-and-promises-nothing-else) | Stop keeps the filesystem and promises nothing else | `Lifecycle.Stop` guarantees the tree, never the processes |
| [§2.7q](#27q-a-sandbox-makes-its-working-directory) | A sandbox makes its working directory | A stock image need not ship one |
| [§2.7s](#27s-apply_patch-locates-hunks-by-whole-lines) | apply_patch locates hunks by whole lines | Context can never bind inside a longer line |
| [§2.7t](#27t-sandbox-file-tools-share-execs-path-view) | Sandbox file tools share exec's path view | Relative under the working directory, absolute as-is; bind-mount confines to `WorkDir` |
| [§2.8](#28-nested-agent-as-tool-attribution) | Nested agent-as-tool attribution | How usage, spans and errors attribute across a nested agent-as-tool |
| [§2.9](#29-budgets-) | Budgets 🚧 | `MaxTurns` is the one budget dimension implemented |
| [§2.10](#210-errors-and-recovery) | Errors and recovery | Stable `ErrorCode`s, and which errors a run can recover from |
| [§2.11](#211-event-fan-out) | Event fan-out | `Fanout[T]`: one producer, many consumers, a drop is a `*GapError` |
| [§2.11b](#211b-run-control) | Run control | `RunControl` — steer, inject, approve, cancel, from another goroutine |
| [§2.11e](#211e-span-coverage) | Span coverage | Which spans are guaranteed, and their parent-child shape |
| [§2.11d](#211d-diagnostics) | Diagnostics | A `Diagnostic` records trouble a run survived |
| [§2.11c](#211c-logging) | Logging | Off by default; what the SDK logs when it is not |
| [§2.12](#212-middleware) | Middleware | `RunMiddleware` wraps a whole run, outermost first; Plan denies, never hides |
| [§2.13](#213-background-tasks) | Background tasks | A task is a sub-agent that outlives the turn that started it |
| [§2.14](#214-the-sdk-reads-no-environment-variable) | The SDK reads no environment variable | `agents` calls no `os.Getenv`; two touchpoints sit outside the rule |
| [§2.15](#215-the-model-adapter-contract) | The model adapter contract | What every `agents.Model` produces: items, stream vocabulary, usage, continuity |
| [§2.16](#216-mcp-client-shared-connections-and-retry) | MCP client: shared connections and retry | A shared connection outlives its callers; a retry waits on the transport, never on an answer |

### 2.0 Entry points

`Run` returns `(RunStream, RunControl)`; `RunSync` returns `(*RunResult, error)`.
`ResumeRun` / `ResumeRunSync` are the same pair for a paused run.

- **A run executes on the consumer's goroutine.** Ranging the stream advances
  the loop; abandoning it stops the run where it stands. The one exception: a
  tool streaming progress yields from its own goroutine ([§2.7g](#27g-tool-progress)).
- **Abandoning the stream cancels the run's own context root**, so the model
  call, the tool batch and racing guardrails are told to stop. A tool that
  ignores its context runs on; its result is discarded.
- **A `RunStream` is single-use.** Ranging it a second time yields a
  `*UserError`: the run body lives inside the iterator, and a second range would
  re-execute it.
- **The result is the stream's terminal event** (`RunCompletedEvent`), emitted
  exactly once on a run that ends without error. A failing run ends with a
  non-nil error and emits no completion.
- **The only behavioral difference between the two entry points is the model
  call**: `Run` streams it, `RunSync` makes one blocking call. There is one loop.
- **A trace always closes**, including when the consumer abandons the stream:
  the deferred trace finish runs when `yield` returns false, and every span it
  opened is finished and exported.

### 2.0b Option grouping

`RunOptions` groups its fields by what they configure — `Model`, `Conversation`,
`Exec`, `Compaction`, `Observe`, `Log`. The zero value stays usable.

- **`Conversation` collects options that constrain each other**: a local
  `Session`, `UsePreviousResponseID` and `ConversationID` are alternatives, not
  layers, and a run that combines a local session with server-managed state is
  rejected.

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

**A `RunState` round-trips whole.** Everything a resume consumes is in the wire
format — the pending injected input, the disclosed deferred tools, the
server-conversation cursor, the off-chain-history flag and the host extra map
(`Extra`) — pinned by a full-field round-trip test (`RunStateSchemaVersion`
1.6). The serialized surface IS the contract; the in-process resume passing the
live pointer is never the only path that works.

- **A resumed turn re-processes a response the restored cursor already
  accounts for and does not advance it**, so a resumed run keeps sending deltas.
- **The state carries the run's full past** — every raw response and generated
  item — so a resumed run's `RunResult` reports the same `RawResponses` (and
  `UsageByResponse()`) as one that never paused. Its size grows with the run.
- **A resumed run is observed exactly like the run it continues**: `resumed`
  marks the log and the trace name, nothing else. Any other difference between
  the two is a bug.
- **`RunState.Extra` is host-owned and opaque**: marshalled verbatim, never read
  by the SDK. It covers pause→resume only; a fact that must survive a crash
  mid-run needs the host's own durable write (`PlanPhase.OnUnlock`, [§2.12](#212-middleware)).
- **The run keeps one item log.** `RunResult.NewItems` and `RunState.SessionItems`
  are that log in full, append-only.
- **`RunState.GeneratedItems` is the suffix of `SessionItems` the model still
  sees** — a projection, never a second record; a resume takes it by length. A
  handoff input filter ([§2.4](#24-handoffs)) or a recompaction
  ([§2.5f](#25f-compaction), [§2.5g](#25g-context-overflow)) folds the log so
  far into the run input and restarts the view at the log's end.
- **A `RunState` decodes across a version window, not on strict equality.**
  `RunStateFromJSON` accepts the same schema major from
  `runStateOldestDecodableMinor` up to `RunStateSchemaVersion`; anything newer,
  another major, or below the floor is a `*UserError` naming which way it
  missed. A minor may only ADD fields — a bump that replaces or reinterprets
  one raises the floor to itself.
- **A consumer triaging stored states applies the same window through
  `RunStateVersionSupported`**, never string equality against
  `RunStateSchemaVersion`.
- **The registry must resolve every agent the state names** — the current
  agent and every agent an item or interruption carries; `RunStateFromJSON`
  fails with a `*UserError` listing the misses rather than leaving any nil.

— see [decisions §5.18](../explanation/decisions.md#518-a-runstate-decodes-across-a-version-window-and-the-window-is-earned)

### 2.1b Items

**`RunItem` is one struct with a `Kind`, not an interface.** The kinds are a
closed set the runner produces — message, tool call, tool output, handoff
call/output, reasoning, injected input, unknown. A stored `RunState` holds
`{type, agent, input, source, display}`, and the struct IS that shape, live and
stored.

- **Consumers switch on `Kind` and treat an unrecognized kind as opaque** —
  render it via `Display()`, never fail — so the set can grow.
- **`Source` says who produced an item.** The zero value is the model; the rest
  name what the runner synthesized (a tool output, a handoff acknowledgement, an
  error handler's fallback) or what the caller injected. Provenance is a field,
  not a sentinel response id.
- **`Display()` is a hint.** A consumer that ignores it must still render
  correctly from the item's own fields, so `ItemDisplay` can gain fields freely.
- **Both survive `RunState` serialization.** A rebuilt item carries `RawInput`
  and its stored display; `Raw` and `Output` are nil — a tool's Go-native
  return value does not round-trip, only its rendered input form does.
- **An unknown output item is kept, never dropped.** An output type this SDK
  does not model becomes an `ItemUnknown` carrying the original bytes and goes
  back on the wire byte for byte on the next turn.
- **Input normalization drops a dangling tool call and a reasoning item with
  nothing following it**, which the API rejects on a resume or replay. A call
  carrying no `call_id` is KEPT — the server decides rather than the SDK
  over-pruning.
- **`UnmarshalInputItem` preserves a typed item the union does not know**, so a
  session written by a newer build stays readable. An item with no `type` is
  still rejected.

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
tool result are persisted, and the next model call has not happened yet. It is
one place in the code, and its step order is the contract:

1. flush the turn to the session
2. ask `ShouldStopAfterTurn`
3. compact ([§2.5f](#25f-compaction)), rebuilding the context from the log
4. drain the steer and next-turn queues ([§2.11b](#211b-run-control))
5. call `PrepareNextTurn`

- **Persist first**: a run that stops at step 2, or whose context is rewritten
  at step 3, has its history already written.
- **Stop is asked before compacting**, against the turn that actually happened.
- **Drain after compacting, never before**, so injected input is never folded
  into the pass that ran before it arrived.
- **A handoff reaches only steps 1 and 2.** The next turn belongs to a
  different agent, resolved fresh, its context rewritten by the handoff input
  filter.

### 2.3b Turn snapshots

A turn is resolved into a `TurnSnapshot` — agent, model, settings,
instructions, prompt, tools, handoffs, output schema, input — before the model
is called, and the turn reads the snapshot from then on, never the agent.

- **`PrepareNextTurn` may return a replacement, which applies to one turn**; the
  turn after resolves afresh, so dynamic instructions still change per turn.
- **A run changes shape mid-flight without mutating the `Agent`**, which a
  concurrent run may be reading.
- **The runner owns `Input`.** A returned snapshot has it replaced with the next
  turn's real input; honoring a prepared snapshot's `Input` would replay the
  previous turn with its tool call and output missing. To edit what a call
  sends, use `ModelOptions.InputFilter`, which runs per turn on the input the
  loop built.

### 2.3c Stopping early

A turn that would otherwise continue can be ended from two places, and only
two:

| Level | Mechanism | Final output |
|---|---|---|
| tool | `ToolResult.Terminate` | the last tool's output |
| run | `ExecOptions.ShouldStopAfterTurn` | the turn's last message, else its last tool output |

- **`Terminate` requires unanimity** across the batch ([§2.7b](#27b-tool-results)).
- **`ShouldStopAfterTurn` is consulted at the save point**
  ([§2.3a](#23a-the-save-point)), at both branches that would take another
  turn, a handoff included: a run stopped there has its full history saved, and
  stopping at a handoff means control never leaves the agent.
- **It is not consulted on a turn that already ends the run.**
- **It is a predicate, not a producer.** The final output is derived from the
  turn; a caller wanting something else computes it from `RunResult.NewItems`.
- **The `*TurnResult` a hook is handed is its own to read.** Writes to its
  fields reach neither the run nor the next hook.
- **Both survive `ResumeRun`**: an approved run carries the same stop policy.
- There is no agent-level early-stop configuration; the policy belongs to the
  run.

### 2.4 Handoffs

- **A handoff is expressed as a function call**; to the model it is just a tool.
- **On the stream it surfaces as both `tool_called` and `handoff_requested`** —
  the model's view and the runner's. The `tool_called` wrapper carries
  `RunItem.IsHandoff = true` and has no paired `tool_output`, so a consumer can
  drop or badge the wrapped form without a list of every handoff tool name.
- **The target resolves from `OnInvoke` when set, else from `Target`** (the
  static declaration `HandoffTo` fills); neither set fails the run with a
  `*UserError`. `Target` keeps the graph statically enumerable for a consumer
  rebuilding an agent registry (`RunStateFromJSON`, an approval UI); a dynamic
  handoff declares itself non-enumerable by leaving `Target` nil.
- **Multiple handoffs in one response → the first wins**, the rest are ignored.
- **Handoff alongside regular tools → all tools execute first**, then the agent
  switches (step 8 of [§2.2](#22-ordering-within-a-turn)).
- **`MaxTurns` keeps accumulating across a handoff**; it is not reset.
- **`InputFilter` may rewrite the history handed to the target agent. The
  session always retains the unfiltered conversation.**
- **The acknowledgement the target reads for the transfer call is the marker
  `{"assistant": <target name>}` followed by a plain-language identity line**
  ([§5.40](../explanation/decisions.md#540-a-handoff-acknowledgement-tells-the-target-it-owns-the-turn)).
- **The target agent's `OnStart` fires at the beginning of the next turn.**
- **A handoff happens inside the same run**, sharing its session and usage;
  agent-as-tool ([§2.8](#28-nested-agent-as-tool-attribution)) starts a nested
  run.

### 2.5 Session persistence boundaries

| When | What is written |
|---|---|
| Just before the first model call | The new user input — deferred so a failure ahead of that leaves no orphan message |
| End of each turn | The items produced by that turn |
| Final turn | **After output guardrails pass** — a tripped final output is never persisted |

- **Whether a tripped input guardrail leaves the user message behind is decided
  by `Blocking` alone.** A blocking guardrail finishes before the save, so a
  tripwire leaves the session untouched; a racing one (the default) trips while
  the model call is in flight, so the input is persisted. Both entry points
  answer identically.
- **A save that leaves nothing behind is announced as `ItemsPersistedEvent`.**
  The implication is one-way: the event guarantees every item the stream showed
  before it is in the store; its absence promises nothing (a run without a
  session, history restored on resume, a save that withheld an interruption's
  pending calls). Consumers mirror persisted state from this event, never from
  raw response events.
- **`safePersistBoundary`: the stored conversation never contains a function
  call without its output.** A run paused for approval withholds the pending
  `function_call` items and writes them with their outputs after resume. The
  guarantee does not survive an abnormal process exit;
  [§2.5h](#25h-crash-recovery) repairs that.
- **Entries are append-only** ([§2.5b](#25b-session-entries)).

### 2.5b Session entries

A session stores **entries**, not bare Responses items: the item plus what the
run knew about it (provenance, display, the model call it belongs to), or
something that is not a Responses item at all (an annotation, a compaction
checkpoint, terminal output).

- **The kind vocabulary is open.** A build that meets a kind it does not know
  ignores the entry rather than failing.
- **Projection decides what the model reads.** Items and compaction checkpoints
  project by default; annotations, terminal output and custom entries do not.
  `RunOptions.Conversation.Projectors` overrides this per kind.
- **A compaction summary projects as a *system* message**, never a user one.
- **A checkpoint copies nothing**; what it holds and how it projects is
  [§2.5f](#25f-compaction).
- **Entries are append-only.** Nothing is rewritten in place. A display settled
  after its turn ended is an **update entry** naming its target, folded in at
  read time: updates apply in stored order, and the last write wins per field.
- **An update may be stored before its target.** Association is by id, so the
  "task finished before the parent turn was persisted" race cannot occur.
- **A tool call is also addressable by its call id** (`TargetCallID`), for an
  amender that knows the call but not the entry id storage has not yet assigned.
- **An update whose target is missing is ignored, not an error.** The target
  may have been folded away by compaction.
- **Folding is a projection: it never writes through to storage.** Readers get
  shallow copies whose `Display` (and its `Extra` map) is shared with the stored
  entry, so the fold copies what it merges — a read never changes the next read.
- **A server-managed conversation (`openai.ConversationsSession`) holds only
  items**; other kinds are dropped on write rather than failing the run.

### 2.5c Session layering

A session is three layers, split along what varies:

- **`session.Storage`** reads and writes entries and understands nothing about
  what they mean.
- **`Session`** is a concrete type, not an interface, that turns entries into
  the model's view. Storage varies; "how history becomes model input" does not.
- **`session.Repo`** owns lifecycles — create, open, list, delete.

Rules across the layers:

- **Reads page on sequence numbers, not offsets.** An offset shifts under a
  concurrent append. A negative `Cursor.Limit` takes the most recent N.
- **Derived state is a fold, never a stored field.** `State` and `Stats`
  recompute from the entries. `State` folds the ACTIVE BRANCH — the view
  recovery reads — so a dangling call on an abandoned attempt is not pending;
  `Stats` stays whole-log.
- **`ContextEntries` is the active branch minus what compaction folded**
  ([§2.5f](#25f-compaction)); `ProjectEntries` applies the same exclusions
  again wherever it is called, so a view built without the filter still cannot
  replay folded history.
- **A branch view is computed from the whole log, and the cursor only trims the
  answer.** The walk follows `ParentID` links back from the leaf, so
  `ContextEntries` reads every entry and pages afterwards; a `Cursor` passed in
  saves nothing on the way in. The cost grows with the conversation and
  compaction does not reduce it — an accepted ceiling.
- **Capabilities a store may or may not have are optional interfaces**, not
  required methods: `AtomicReplacer`, `GuardedReplacer`, `CompactionAware`.
- **A wrapper that claims a capability delivers its contract or refuses.**
  Delegating `AtomicReplacer` to a wrapped store without it returns an error
  before touching anything, never a non-atomic Clear+Append; `GuardedReplacer`
  over a store that cannot compare the log back errors rather than answering
  `replaced=false`.

### 2.5d Sessions are trees

An entry names its parent, so a session is a walk rather than a pile.

- **Branching abandons without deleting.** Moving the active branch leaves the
  old attempt recorded and off the path.
- **Switching branches is an append** (`EntryKindLeaf`), not a mutable pointer;
  the current leaf is derived by folding the log.
- **The model sees the active branch**, not append order.
- **Parent links are assigned by storage**, the only layer that knows the ids
  it is about to mint; `PrepareAppend` is shared so every backend links
  identically.
- **A linkless run of entries is one straight line, and the branch continues
  through it.** Entries with no parent ids and no leaf moves (a pre-branching
  session, a server-held store) cannot be off-branch; the walk to the leaf is
  extended over the linkless prefix ahead of its root. Every branch-scoped view
  (`ContextEntries`, `PathEntries`) shares this rule through `ActiveBranchOf`.
- **A walk does NOT stop at a compaction checkpoint.** The walk answers "which
  entries are on this branch"; what the model sees is projection's question. A
  missing parent ends the walk, and a repeated id does too, so a corrupt
  session reads short instead of hanging.
- **Fork extracts a branch; branch moves within one session.** A fork carries
  entry ids across unchanged, so an update entry naming one still finds its
  target, and writes through `session.ReplaceEntries`, so an `AtomicReplacer`
  never shows a cleared-but-unfilled fork target.

### 2.5e Session lifecycles

A `session.Repo` owns which sessions exist, separately from their contents.

- **Existence is recorded, not inferred.** A session created with no entries is
  still listable.
- **`Hidden` belongs to the session**, not to each caller's filter. A session
  serving another one (a background task's history) is excluded from listings
  by default and is created naming the session it serves
  (`CreateOptions.ParentID`); a repo may ignore the name or refuse an unknown
  one with `ErrNotFound`, and never creates the parent.
- **A backend may constrain the shape of the ids it accepts** (the server's
  PostgreSQL backend types every id column `uuid`); the conformance suite routes
  its literal names through `RepoUnderTest.IDs` for such a backend.
- **Opening an unknown session is an error**, never an empty session.
- **Deleting removes the entries with it**, atomically where the backend can.
- **A name that two ids share belongs to whichever id claimed it.** A backend
  mapping ids onto a narrower namespace (a filesystem) records the original id
  and refuses to open or delete through the other one. **A check that cannot
  be made refuses.**
- **A failed create leaves nothing behind.** A backend that claims a name
  before it can finish writing gives the claim back if the rest fails.

### 2.5e2 The entry lifecycle contract

What happens to an entry over its life — minted, addressed, walked, removed.
**None of it is a backend's decision**: each item names who implements it, and
where that is *shared*, a backend answering the question itself is a bug even
when its answer is right.

#### Identity

- **A session id names a session, not a place.** Deleting an id and creating it
  again yields a session with storage of its own; a handle to the deleted one
  can neither read what its replacement writes nor write into what it reads.
  *Shared:* `session.Ref` is the address, `session.NewGeneration` mints the
  discriminator, and `session.NewSessionID` mints an id for a `Create` that
  supplied none. A function that takes a ref cannot be handed a bare id.
- **A handle is bound when it is BUILT**, never on first use. *Shared.*
- **A constructor where the id names the STORAGE** (`sessions.New`) keeps its
  meaning: opening it twice is the same conversation, and it shares storage
  with a repo's sessions in neither direction. *Per backend.*
- **A lookup that failed is not an answer.** "This id has no session" is a
  fact; a cancelled context or an unreachable store is a failure to look, and
  is never resolved to absence. *Shared.*

#### Entry identity

- **An entry id is unique within its session and never handed out twice**,
  including after the entry holding it is gone — so it is never minted from the
  stored count. *Shared.*
- **Ids are opaque.** Nothing outside the minting code constructs or parses
  one; a caller that needs an entry reads its id back. *Contract on callers.*
- **A backend that can constrain them does**, so a collision is a failed write.
  *Per backend.*

#### Sequence numbers

`Seq` is a **cursor position**, and that is the whole of its meaning.

- **Monotonic within a session**, and the ONLY order a backend reads history
  in — a row's own time-ordered key (a UUIDv7) is not, since a clock can step
  back.
- **Never reused**, including after the entry holding it is removed.
- **Never moved for an entry that stays.** The one exception is a rewrite that
  RE-ADDS an entry (server-side compaction carrying over what it did not
  summarize): ids are kept, numbers are fresh, so a consumer tailing with
  `AfterSeq` sees it again under a new number and deduplicates by id.
- **`Clear` and `ReplaceEntries` do not restart it.** A cursor outlives the
  entries it pointed at.
- **One value per entry, whichever API returns it.** *All shared, but for the
  backend below.*
- **A server-held conversation numbers best-effort.**
  `openai.ConversationsSession` numbers by position in what the server
  returned; a negative limit is unaffected, resuming from `AfterSeq` is
  best-effort. *Per backend.*

#### The change record

- **Every change moves a session in its listing**, clearing included.
- **It never moves backwards**, so it is never inferred from stored content.
- **A session with no writes yet sorts by when it was created.**
- **A session's metadata and the listing's are the same answer**, read through
  one path. *Shared.*
- **A listing is that record's order, newest first**; `ListOptions.Limit` cuts
  from the newest end, after the hidden filter, and a non-positive limit is no
  limit. It is a plain count, not a `Cursor`. Sessions sharing a time may come
  back in either order. *Shared contract (`internal/agentstest.RepoConformance`);
  ordering per backend.*

#### What must be one step

- **A write and the record that it happened.** Where a backend genuinely
  cannot (two files), the record is best-effort and its failure is NOT
  reported.
- **Reading the append point and writing against it.** A transaction alone is
  not one step (under read committed both writers read the old tip): a lock
  over read-and-write, a transaction owning the pool's single connection
  (SQLite), or a transaction-scoped advisory lock (PostgreSQL). The unique
  constraints are the backstop for POSITIONS; the fork itself only the
  serialization prevents.
- **Reading what is being removed, then removing it.** An undecodable record is
  still the only copy of what it holds.
- **Claiming and acting.** The check that a session is still the one meant is
  part of the delete, never a step before it.
- **Selecting a row to remove and removing it.** A caller whose delete affects
  nothing lost the race and retries.
- **Writing and proving the destination still exists.** A handle held across
  its session's deletion REFUSES the write (`session.ErrNotFound`) inside the
  same step as the write. Deletion honors the same serialization as writes,
  and where the proof is a row, that row is deleted FIRST.
  *Shared contract; mechanism per backend.*

#### Absence

- Only "there is no such thing" is absence. Every other failure reaches the
  caller. *Shared.*

### 2.5f Compaction

Compaction is a **run-level** concern (`RunOptions.Compaction`): deciding what
to drop needs the model, the usage numbers and the context window, all of which
belong to the run.

- **Nothing is deleted.** A strategy marks groups excluded and may leave a
  folded replacement; the log stays whole and the model's context is a
  projection of it.
- **The run consults its `Compactor` at three points**, all three by default:

  | Point | When |
  |---|---|
  | `CompactBeforeRun` | after reading the session, before the first model call |
  | `CompactAtSavePoint` | at each turn boundary, after the turn is persisted |
  | `CompactAfterRun` | once the final output is persisted — only for a `Compactor` that is a `CompactionCheckpointer` |

- **A save-point pass rebuilds the context from the log**, never editing the
  items in flight.
- **Compaction never fails a run.** A failed pass is recorded on the
  `compaction` span and the run continues with the entries it had.
- **"Did the pass change anything" compares whole entries**
  (`session.Entry.Equal`), not the count and not a subset of fields. A pass
  judged a no-op is discarded: the save point does not rebuild, and the
  after-run point writes no checkpoint.
- **An incremental index resumes only on an exact prefix**, compared the same
  way, token usage included; entry ids are unique per session, not globally.
- **The size estimate is the newest measured usage, minus this pass's
  unsettled exclusions (their replacements added back), plus an estimate of
  everything after it**; with no usable usage anywhere it estimates
  everything. An exclusion is `settled` once a later model call has priced it
  in — new entries mean that call measured the view without it — and is no
  longer subtracted.
- **Local compaction and server-held history do not interact**:
  `UsePreviousResponseID` / `ConversationID` already refuse a local `Session`.
- **A self-compacting storage (`CompactionAware`) takes the `CompactAfterRun`
  point instead**; the two never both run on one session.

**A checkpoint is appended, never a rewrite — and it copies nothing.**

- **`CompactAfterRun` records the pass as an `EntryKindCompaction` entry**
  whose payload names the entries it folded (`ExcludedIDs`) and carries only
  what exists nowhere else: the summary text and a `CompactionFold` per folded
  group, whose stand-in renders in the group's place (anchored `Before` the
  first surviving entry after it). Kept entries are read from the session,
  never from a copy inside the checkpoint.
- **Folded entries stay in the session untouched**, so a reader can offer to
  expand them and a fork from before the checkpoint still finds its full
  history. `ContextEntries` leaves them out and `ProjectEntries` renders each
  live checkpoint's summary up front, so the next run reads the shorter context
  without recomputing the pass.
- **Writing a checkpoint is an optional capability** (`CompactionCheckpointer`).
- **A checkpoint is bound to its pass.** `Checkpoint(seen)` names the entries
  the caller's own `Compact` saw; a compactor whose state no longer describes
  them (shared across concurrent runs, re-aimed at another session) reports
  nothing rather than recording another conversation's exclusions.
- **The one path that still rewrites is `openai.CompactionSession`**, whose
  server compact API returns a replacement rather than a decision.

**A rewrite built from the response chain never deletes what that chain never
saw.** The runner reports every way the log outgrows the chain through the
single flag `CompactionArgs.OffChainItems`:

| Case | Reported when |
|---|---|
| Position | anything stands AFTER the last model-produced item — a terminating tool's output, an error handler's fallback, input injected past the last model call. Decided by position, never by provenance |
| A truncated read | `Conversation.Settings.Limit` is set and the prepare-time read came back FULL (a log exactly the window's size reads full too; the rule errs toward reporting) |
| A handoff input filter | a filter RAN, whatever it returned; its output is never inspected |
| A projector that withholds an item | a projector returned nothing for an `item` entry — measured per entry, not per config; a projector that rewrites an item is not withholding it |

- **The last three ride across an interrupt/resume on
  `RunState.OffChainHistory`**; a resumed run re-reads no history and re-runs
  no filter. Position clears between runs, so it is recomputed every time and
  never carried.
- **`openai.CompactionSession` answers the flag by compacting from the stored
  items** instead of `previous_response_id`. A caller who PINNED
  `CompactionModePreviousResponseID` gets the pass skipped and
  `abandoned: off_chain_items` on the span — transient for position, every run
  past a truncating window (a conflict only the caller can resolve).
- **The runner never decides this by skipping the pass**: a storage with no
  chain to be wrong about must still compact.
- **That rewrite is guarded by the sequence number it read.** The swap goes
  through `GuardedReplacer`: the store compares its highest HELD sequence
  number (zero for a log read empty; never the highest ever issued) and writes
  only while it still matches, comparison and write in ONE step under whatever
  already serializes that store's appends.
- **A pass that loses the comparison is abandoned**, not retried and not
  merged: nothing is written, `abandoned` is recorded on the `compaction` span,
  and the next pass starts from the history as it then stands. A store without
  the capability keeps the unguarded swap.
- **The rewrite keeps the ids of the entries it carries over**, so an update
  entry still finds its target; those entries are numbered afresh
  ([§2.5e2](#25e2-the-entry-lifecycle-contract)).

— see [decisions §5.51](../explanation/decisions.md)

### 2.5g Context overflow

Compaction predicts; overflow recovery reacts where the prediction was wrong.

- **`ExecOptions.Overflow.MaxRetries` enables "compact, then try this turn
  again". Zero by default**: an overflow is reported, never silently shrunk
  away.
- **The retry does not spend the turn budget.** The budget counts model calls
  the model made; an overflow is one it never got.
- **A no-op compaction buys no retry.**
- **Retries are counted across the run**, not per turn.
- **Recovery writes the turn to the session before retrying.** It rebuilds the
  turn's context from the log and throws the in-flight items away, and a steer
  drained at the save point is counted delivered by the next write past its
  mark ([§2.11b](#211b-run-control)).
- **The write lands on the side of the pass the pass can survive.** With a
  `Compactor` the turn is written BEFORE the pass; with a `CompactionAware`
  storage it is written AFTER. Which path applies is decided up front, from
  whether the storage compacts itself.
- **The write obeys the usual boundary** — a batch ending in a call without its
  output stays held back — **and a write that FAILS abandons the recovery**:
  the overflow is reported, a `compaction_failed` diagnostic records why, and
  the take stays in flight for the rollback to re-queue. A run with no recovery
  available (no `Compactor` at this point and a storage that does not compact
  itself) writes nothing.
- **A self-compacting storage recovers too.** With no run-level Compactor (or
  one standing aside), recovery calls the storage's `RunCompaction` with
  `Force: true` and rebuilds the turn's context from the session.
- **A forced pass buys a retry only if the context came back weighing strictly
  less** — the summed byte length of the entries' stored bodies over the same
  windowed read the model is handed. Neither the entry count nor "did anything
  change" decides it: a saturated window hides growth, and an unchanged
  history weighs what it weighed.
- **Detection matches the provider's message** — a 400 with prose in it; any
  other 400 is a malformed request, not an overflow. The marker list covers
  OpenAI's `context_length_exceeded` family and Anthropic's "prompt is too
  long" / `model_context_window_exceeded`.
- **A backend may report overflow in a SUCCESS-shaped response** (Anthropic's
  `stop_reason: model_context_window_exceeded`); the adapter surfaces it as an
  error carrying the marker ([§2.15](#215-the-model-adapter-contract)).
- **A truncated response is NOT an overflow**
  ([§2.7e](#27e-truncated-responses)): its input fit.
- A recovered overflow is recorded as a `context_overflow` diagnostic.

— see [decisions §5.52](../explanation/decisions.md)

### 2.5h Crash recovery

`session.Recover` repairs a session a killed process left inconsistent.

- **The damage is specific**: a run killed between issuing a tool call and
  recording its output leaves a `function_call` with no `function_call_output`,
  which the Responses API rejects outright — the session is **unloadable**.
- **The repair appends**: a synthesized error output is added and nothing is
  rewritten.
- **The synthesized output says what happened** and warns against assuming the
  tool succeeded.
- **An unfinished call is never retried by default.** Only a tool declaring
  `RetrySafe: true` is left dangling for the next run to redo.
- **`RecoveryPolicy.RetrySafe` is supplied by the caller**, since the stored
  history holds a tool NAME and only the caller knows the agent;
  `RetrySafeNames(tools)` builds it.
- **It is the counterpart of `RunState`, not a replacement**: `RunState`
  handles a run that paused on purpose; this handles a process that died and
  left only what had been written ([§2.5](#25-session-persistence-boundaries)).

### 2.6 Guardrails

One `Guardrail` type covers every stage. Placement decides scope: guardrails in
`RunOptions` or on an `Agent` apply to the whole run — their tool stages cover
every tool that agent exposes — while guardrails on a `Tool` apply to that tool
only.

| Stage | When | Decision space |
|---|---|---|
| `input` | First turn, before the model call (`Blocking`) or concurrently with it (default) | Allow / Replace / Trip |
| `output` | After the final output is produced, before persistence | Allow / Replace / Trip |
| `tool_input` | Before execution, on the raw argument JSON | Allow / Replace / Trip |
| `tool_output` | After the tool runs, before the result is fed back | Allow / Replace / Trip |

**Ordering, concurrency and cancellation:**

- **Input and output stages run their guardrails concurrently and fail fast**:
  the first tripwire or error ends the wait and cancels the rest.
- **Tool stages run in order and stop at the first `Replace` or `Trip`.**
- **A non-`Blocking` input guardrail runs concurrently with the model call, and
  a tripwire cancels the in-flight call**: it is not billed and produces no
  response event.
- **A model call that fails on its own cancels the racing guardrails.** A
  verdict already delivered still wins; the guardrails' own cancellation error
  never masks the model's error.
- **A panicking guardrail is recovered into an error that aborts the run.**
- **Every consulted guardrail produces a result**, allowing decisions included.
- **Streaming and blocking share one guardrail behavior**: there is one loop.

**`Replace` semantics:** the decision's `Message` replaces the inspected content.

- `input` — appended as a single user text message replacing the original
  input (for finer rewriting use `ModelOptions.InputFilter`, which edits the
  exact items sent without changing what is saved).
- **A `Blocking` input guardrail's replacement reaches the model on the guarded
  call itself**: the turn's input is rebuilt from it before the call. A racing
  guardrail's replacement misses the call it raced and applies from the next
  turn on.
- **A replacement that cannot apply fails the run** with a `*UserError`:
  server-managed turns (`UsePreviousResponseID`, a server-held conversation)
  send only deltas and cannot rebuild from the input — use a local session, or
  `Trip`.
- **Racing never de-streams the call**: the raced model call still yields raw
  events on the consumer's goroutine; a tripwire cancels it mid-stream, and
  events already yielded stand.
- `output` — replaces the final output value.
- `tool_input` — the tool does not execute; `Message` becomes its result.
- `tool_output` — replaces the content returned to the model.

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
- `Agent.ApproveTools` lists tool names (or `"*"`) that need approval whatever
  the tool itself says. Precedence for one call: a decision already recorded
  on the `RunState` (approve/reject, per call or per tool) settles it and
  nothing else is consulted; else the tool's own answer (`NeedsApprovalFunc`,
  then `NeedsApproval`); else the agent's listing. The listing only ever adds
  a pause — it cannot exempt a tool that asks for one.
- If **any** call in a turn needs approval, the whole turn pauses
  (step 4 of [§2.2](#22-ordering-within-a-turn)).
- Approval decisions may be scoped ("this call", "all calls to this tool", …);
  the caller expresses the scope on the `RunState`.

### 2.7b Tool results

A tool returns a `ToolResult`, not a bare value; a plain value (string, struct,
`ToolOutputContent`) is wrapped automatically. Some of what a tool knows is
**not for the model**:

- **`Content` reaches the model. `Details` never does** — it lands on the
  item's `Display().Extra`, for the UI and for logs.
- **`Title` and `Summary` are display overrides** on the same side: a card
  heading and a one-line account. Empty means fall back; neither is ever
  required.
- **`Details` must survive a JSON round-trip.** A value that cannot fails the
  run at the tool call, not at persistence time. An empty map normalizes to
  nil.
- **`Usage` accounts for model calls the tool made itself**, so nested spend is
  attributable to the call that caused it.
- **`Terminate` requires unanimity.** The run stops after a batch only when
  every tool in it asks.
- **`IsError` marks a failure for renderers; the content still reaches the
  model.** A tool error handled by `FailureErrorFunction` sets it automatically.
- **A multimodal output displays as the wire content list.** `Display().Output`
  of a `ToolOutputContent` / `[]ToolOutputContent` result is the JSON of the
  Responses `function_call_output` content list (`input_text` / `input_image` /
  `input_file` items), never this package's Go types — the same on the live
  stream and in the stored entry.

### 2.7c Tool capabilities are fields

`*Tool` is the only tool type, and everything a tool can do beyond being called
is a **field** on it: `OnInvoke`, `Description`, `ParamsJSONSchema`, `Strict`,
`NeedsApproval` / `NeedsApprovalFunc`, `Guardrails`, `Timeout`, `Sequential`,
`IsEnabled`, `ReadOnly`, `FailureErrorFunction`, `Deferred`, `RetrySafe`.

- **The runner reads them directly.** There is no capability lookup; a tool's
  timeout is `tool.Timeout` whether the tool was built here or elsewhere.
- **Adapting a tool you did not build is copying the struct.** The schema map
  and validator are built at construction and never mutated, so a copy shares
  them safely ([howto/tools.md](../howto/tools.md)).
- **`Guardrails` is appended to, never assigned**, on a tool that already
  declares its own.
- **A hook composes only if captured before it is overwritten**
  (`inner := tool.IsEnabled`); overwriting without capturing is a legal
  replace.
- **Copying never changes what the model is told** unless the copy assigns to
  `Name`, `Description` or `ParamsJSONSchema`.
- **"Errors abort the run" is `FailureErrorFunction = nil`** — an absence a
  field can express.

— see [decisions §5.4](../explanation/decisions.md#54-a-tool-is-a-struct-not-an-interface)

### 2.7d Tool-loop safety valves

The loop's own failure modes, where an agent keeps going and gets nowhere:

- **Consecutive all-failed turns abort the run.**
  `ToolLoop.MaxConsecutiveErrorTurns` (default 3) counts TURNS in which *every*
  tool call failed; any success clears it, and a turn with no tool calls is
  neither counted nor cleared. A negative value disables it.
- **`ToolLoop.FinalTurnWithoutTools` is opt-in.** With it, an exhausted turn
  budget buys one more model call **with no tools and no handoffs**, so the
  model closes out in prose. Opt-in, since the budget may be a cost ceiling.
- **One `Sequential` tool serializes the whole batch.**

### 2.7e Truncated responses

A response the provider marks `status="incomplete"` with reason
`max_output_tokens` was cut off at the output-token limit.

- **None of its tool calls execute** — handoff calls included. Each is
  answered with an explanation that the response was truncated and the call
  was not run, so the model resends; a refused handoff switches no agent.
- **Truncation is not failure.** It is fed back to the model; every other
  incomplete reason still fails the run.
- **Both model paths report it**: `Status` reaches the loop from the blocking
  call and the stream alike.
- **None of its tool calls PAUSE, either.** The guard runs before the approval
  partition, and `Status`/`IncompleteReason` survive `RunState` serialization
  so a cross-process resume refuses the same calls.

A stream that dies before its terminal event is a severed stream, not a
truncated response:

- **A severed connection is a transport failure.** `modelkit.RetryableError`
  treats `io.ErrUnexpectedEOF` as retryable, alongside `net.Error`.
- **An adapter surfaces a clean EOF without a terminal event as
  `modelkit.TruncatedStreamError`**, which wraps `io.ErrUnexpectedEOF` so the
  rule above applies; a transport error AFTER the terminal event is not
  surfaced. The runner's "ended without a completed response" check stays as
  the last line of defense.
- **The commit window closes at the first output event.** `NewRetryModel` and
  `NewFallbackModel` replace an attempt only while nothing has been generated:
  lifecycle preamble (`response.created`, `.in_progress`, `.queued`) and
  terminal failures (`error`, `response.error`, `response.failed`) carry no
  output; `response.incomplete` is output and commits.
- **Pre-commit events are buffered, not delivered.** An abandoned attempt's
  pending events are dropped, leaving a `model_retry` span and a
  `DiagModelRetry` diagnostic; they flush when the attempt commits or is the
  stream's last word. Post-commit errors pass through, recorded as
  `DiagStreamError` by every decorator that saw them.
- **A nil event neither commits nor buffers**; a consumer that stops mid-flush
  ends everything — no further events, no diagnostics.
  — see [decisions §5.16](../explanation/decisions.md#516-a-severed-stream-retries-only-before-output-with-the-preamble-held-back).

### 2.7f Usage attribution

- **Exactly one entry per response carries that response's `Usage`.** Several
  entries share a response; summing over a session must not multiply the bill.
- **It lands on the LAST entry of the response**, so a reader takes the most
  recent measured input+output as fact and estimates only what follows.
- **A turn persisted in two batches (an approval pause) attributes on the
  first** and clears the flag.
- **A backend that returns no response id still has its usage recorded**, on
  the batch's last entry.
- **Injected input carries no response id and takes no usage.** An
  `ItemInjectedInput` entry has an empty `ResponseID`, and attribution skips a
  `SourceUser` entry even when it closes the batch.
- **Nested usage is separate.** `session.Entry.NestedUsage` /
  `RunItem.NestedUsage` hold what a tool spent on model calls of its own; it is
  not merged into `Usage` (a different conversation) but IS part of the run
  total.
- `RunResult.UsageByResponse()` and `RunResult.NestedUsage()` read it back.

### 2.7h Schema validation

Tool arguments, handoff input and structured outputs are validated against the
**whole** JSON Schema, not a root-level `required` check.

- **Nested `required`, nested type mismatches, enums and bounds are enforced.**
- **Errors carry a JSON-pointer path.**
- **Schema `default` values are applied before decoding** — to tool arguments
  only, never to handoff input: `OnHandoff`, `OnInvoke` and the session all see
  the model's raw argument string.
- **`additionalProperties: false` is sent to the provider but not enforced
  locally.** An unexpected key is dropped by Go decoding; a misspelled key is
  still caught by `required`.
- **A schema this SDK cannot compile skips validation** rather than failing.
- **Schemas are compiled once per tool; a handoff's once per invocation** (the
  runner validates a per-turn copy of the `Handoff` value).
- **An agent tool is a tool.** `AsTool`'s `{"input": string}` and
  `AgentAsTool`'s reflected schema face this check before the nested run
  starts, `InputBuilder` or not; failing arguments come back as a
  `*ModelBehaviorError` for the calling model.
- **Handoff input carries two rules the schema cannot express**: arguments must
  be a JSON object, and absent arguments (`""`, `"null"`) read as `{}`,
  rejected with "Handoff function expected non-null input, but got None" when
  the schema declares root-level `required` keys. Both survive an uncompilable
  schema; a nil schema skips validation entirely. A rejected handoff input
  fails the run rather than being fed back.
- `EnsureStrictJSONSchema` is the OpenAI strict-mode *transformer*, a
  different job from validation.

### 2.7i Progressive tool disclosure

A tool marked `Deferred: true` is withheld from the model until some
`ToolResult.AddedTools` names it.

- **Marking the tool is the opt-in**, not a run-level flag.
- **Disclosure is cumulative** for the rest of the run.
- **It survives a resume** (`RunState.DisclosedTools`), a serialized
  cross-process one included ([§2.1](#21-the-run-loop)).
- **It does not override `IsEnabled`.**
- **Naming an unknown tool is ignored.**

### 2.7g Tool progress

`ToolContext.Emit` pushes a partial result to a streamed run's consumer as a
`ToolProgressEvent`.

- **Progress is not the answer.** It never reaches the model; the tool's return
  value does.
- **Scope is the call.** After the tool returns, `Emit` is ignored.
- **No-op on a blocking run.**
- **`emit` serializes with the run loop's own yields**: an iterator's `yield`
  is not safe for concurrent calls.
- **The consumer's range body runs on the emitting tool's goroutine** for a
  progress event — never concurrently with itself, but not on the goroutine
  that started the range. A consumer that pins work to the starting goroutine
  hands the event off ([streaming.md](../howto/streaming.md)).
- **A nested agent-as-tool run is streamed whenever the parent is**; only its
  messages are forwarded, never its raw deltas.
- The sandbox exec tool streams stdout through it, capturing in parallel.

### 2.7j Sandbox command policy

`CodeToolConfig.Policy` filters commands **before** the approval gate.

- **Before, not after**: the veto wraps the tool's own `NeedsApprovalFunc`, so
  a policy-refused command answers "no approval needed" and is refused as text
  by the tool itself — a person is never asked about a command that was never
  going to be allowed.
- **`Deny` is checked after `Allow`, so a deny always wins.**
- **A refusal is a tool result naming the rule**, not an error.
- **The zero value allows everything; a policy whose patterns do not compile
  refuses everything.**
- **It is a filter on approval noise, not a security boundary.** A pattern
  matches the TEXT of a command and is blind to shell semantics
  ([howto/sandbox.md](../howto/sandbox.md)); containment is the sandbox
  backend's job.

### 2.7k Persistent shells

`exec_command` optionally reuses a named shell, so `cd`, exported variables and
an activated environment survive between calls.

- **Completion is detected with a random, per-session sentinel**, carried on
  the command line in two halves and joined only by the output, so neither the
  PTY's echo nor a command printing the token can end a read early.
- **The echo is stripped as an exact prefix** of what was written, never by
  pattern.
- **A timed-out session is closed, not reused.**
- **The output awaited for the sentinel is bounded**: a flooding command's
  middle is dropped, keeping the head and the live tail; the model's result is
  truncated as usual.
- **A session command that fails still returns the output it produced** —
  echo-stripped and truncated — alongside the error, like the one-shot path.
- **Closing the pool preempts a command in flight, and is final.** The
  interrupted command returns an error, never a fabricated exit status; a named
  command arriving afterwards fails rather than opening a shell on a sandbox
  its owner has let go of.
- **Two callers racing the same new name end with exactly one shell**: the
  loser closes its own and takes the winner's.
- **The named shells belong to the tool, not the run.** Two concurrent runs of
  the same agent share one pool; a host wanting isolation builds the tool per
  run.
- **The schema advertises `session_id` only when Sessions is enabled.** A
  `session_id` sent anyway (non-strict backend) decodes and is ignored.

### 2.7l Sandbox tool argument decoding

`exec_command` decodes its own arguments — it is a hand-built tool, not a
`NewTool` wrapper, so nothing upstream of `OnInvoke` catches a malformed call.

- **Malformed arguments are refused as text, not returned as an error.** The
  refusal is an `IsError` result carrying the decode error, the same shape as a
  policy veto; an error return stays reserved for sandbox *infrastructure*
  failure (a dead daemon, a broken connection), which aborts the run
  ([§2.7](#27-tools)).
- **The optional string arguments (`workdir`, `session_id`) accept only the
  zero-value sentinels** `null`, `0` and `false`, each decoding to `""`; any
  other non-string scalar is refused as text. The schema still advertises plain
  `string`, and `cmd` stays an ordinary string
  ([howto/sandbox.md](../howto/sandbox.md)).
- **The approval gate treats undecodable arguments like a policy veto**
  ([§2.7j](#27j-sandbox-command-policy)): a call `OnInvoke` will refuse as text
  never reaches a human.

### 2.7m A sandbox reports its own timeout, never the caller's ending

- **`ExecResult.TimedOut` means one thing: the process was killed for
  exceeding `ExecRequest.Timeout`.** Every backend — local, docker, e2b —
  answers the same way.
- **The caller's ending is checked first.** The per-command deadline derives
  from the caller's context, so a context that ended for the caller's own
  reason (cancelled, or a deadline the caller set) is returned as that error
  bare (`ctx.Err()`), never wrapped in a transport failure, with no result.
- **A timed-out result carries whatever output was collected before the
  kill**; a failure reading the tail costs output, never the `TimedOut`
  verdict.

### 2.7n A sandbox's environment is part of its container identity

- **`Options.Env` (docker) sets variables on the CONTAINER**, so
  `exec_command`, a persistent shell and a terminal opened into it read the
  same values. `ExecRequest.Env` overrides an entry for one call only.
- **A named container is adopted only against a configuration fingerprint.**
  Creation stamps a label hashing every security-relevant option — image,
  runtime, user, network, bind source, resource limits, environment — over
  EFFECTIVE values, so equivalent spellings of one configuration still adopt.
- **Only the label decides ours-vs-foreign.** A container without it is a hard
  error naming the remedy (remove or rename it); one carrying the label with a
  different fingerprint is ours from an older configuration and is
  **replaced** (removed, recreated), never adopted and never an error.
- **Files under the mounted `/workspace` survive a replacement**; anything
  installed into the container's own filesystem does not.
- **A sandbox with no environment set hashes as though the option did not
  exist.** An empty map and an absent one are the same container, and an
  existing container's fingerprint is frozen.

— see [decisions §5.19](../explanation/decisions.md#519-a-named-container-is-adopted-only-against-a-configuration-fingerprint)

### 2.7o A docker sandbox runs as the image's user and joins no network

- **`docker.Options.User` empty means the image's own user** (root for most
  images); the backend applies no user of its own, and a narrower one is named
  explicitly (`"65534:65534"`, `"1000:1000"`). There is no separate "really
  empty" flag ([decisions §5.33](../explanation/decisions.md)).
- **`docker.Options.Network` empty means no network at all**
  (`--network none`); any other value is the docker network to join.
- **Both are part of the adoption fingerprint**
  ([§2.7n](#27n-a-sandboxs-environment-is-part-of-its-container-identity)).
- **No network is the default across backends.** `sandbox/e2b` sends
  `allow_internet_access` on every create — `false` unless the caller opts in
  ([decisions §5.37](../explanation/decisions.md)).

### 2.7p Stop keeps the filesystem and promises nothing else

- **`Lifecycle.Stop` guarantees exactly one thing: the working tree survives.**
  Whether processes do is the backend's business; nothing may depend on a
  process outliving a Stop, and no caller reports one as "still running".
- **`Start` after a `Stop` is a resume of the FILES, not of the work.**
  `Status` reports `absent`, `stopped` or `running`; none says anything about
  the storage, which outlives all three.
- **A backend that cannot control its compute does not implement `Lifecycle`**
  (`LocalSandbox`); callers discover that by type assertion.
- **A by-name lifecycle call never touches a container it did not create.**
  `docker`'s `Stop` and `Status` (like `StopManaged`/`RemoveManaged`) verify
  the ownership fingerprint first — a foreign holder of the name is an error —
  and `Stop` then acts on the resolved id, not the name.

### 2.7q A sandbox makes its working directory

- **A backend creates its working directory on a sandbox it provisioned**
  rather than requiring the image to ship one: `docker` gets this from the
  daemon, `sandbox/e2b` does it itself on the sandbox it created.
- **It is done on the FIRST use, not on every one**: a resumed sandbox already
  has the directory, and a caller that removed it meant to.

### 2.7s apply_patch locates hunks by whole lines

- **An Update hunk's context matches whole lines only**: a match begins at the
  start of the file or of a line and ends at the end of the file or of a line.
  The first line-anchored occurrence at or after the previous hunk's end is
  the one edited.
- **The `@@` anchor binds the same way, with one tolerance**: when no line
  matches it exactly, the first line whose space-trimmed text equals the
  trimmed anchor is taken.
- **`*** Move to:` naming the section's own path is a plain update**, not a
  duplicate-section conflict.
- **A Delete of a file too large to snapshot (`ErrReadLimitExceeded`) is
  parked, not refused**: renamed beside itself (`.apply-patch.<name>.<random>`)
  for the commit, renamed back on rollback, removed last once every operation
  has landed. A parked copy that will not go is reported in the tool's result.
  Update and Move still need the content and fail on such a file
  ([decisions §5.48](../explanation/decisions.md#548-apply_patch-parks-a-large-file-instead-of-snapshotting-it)).
- **One section per path.** Two `Update` sections for one file are refused,
  and so is a rename onto a path another section touches: the plan phase reads
  every original before any write, so the second would compute from pre-patch
  content and silently overwrite the first. `Delete` + `Add` of one path is
  the full-rewrite idiom and passes.
- **Anchors tolerate the git form.** `@@ -a,b +c,d @@ heading` is accepted
  with the range dropped; a bare range is an empty anchor and the hunk locates
  by context. An empty line inside a hunk is an empty context line when hunk
  body follows, else a separator.

### 2.7t Sandbox file tools share exec's path view

The sandbox file operations (`ReadFile`, `WriteFile`, `CreateExclusive`,
`ListDir`, `RemoveFile`, `Rename`) resolve paths exactly as `exec_command`
does.

- **Shell semantics**: a relative path resolves under the working directory,
  an absolute path is used as-is. There is no second path universe rooted at
  `WorkDir`.
- **`ReadFile` behaves like the OS everywhere**: it follows symlinks to the
  file they name and fails on a directory with an is-a-directory error.
- **A missing path is `fs.ErrNotExist` from every operation, on every
  backend**; the in-container docker scripts report absence by exit code,
  never by sniffing a shell's wording.
- **`ListDir` promises no order**; `list_files` sorts by name.
- **`Rename` exists for `apply_patch`'s parking**
  ([§2.7s](#27s-apply_patch-locates-hunks-by-whole-lines)).
- **Persistent-mode docker runs every file operation through `exec`**, never
  the daemon's archive API (`docker cp`), which cannot see a `tmpfs` or volume
  mount: a base64 round-trip over `sh -c` (`wc -c` size-guarded on read, an
  `ExecRequest.Files` stage-and-move on write) keeps one view.
- **Docker bind-mount mode is the one exception**: file operations run on the
  host side of the mount and are confined to `WorkDir` via `os.Root` (which
  polices `..` and symlink escapes). An absolute path must lie under the
  in-container mount point (`/workspace`) and is translated to its host-side
  name; anything else fails with `sandbox.ErrOutsideWorkDir`, never a silent
  re-rooting.

— see [decisions §5.14](../explanation/decisions.md#514-sandbox-file-tools-share-execs-path-view)

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

- **Every SDK error carries a stable `ErrorCode`**, read with `CodeOf(err)`,
  which unwraps `%w` chains.
- **The code is derived from the error's type — exactly one classification.**
  The typed errors carry their data fields and no code field.
- **A run that fails after its loop started returns a `*RunError`** wrapping
  the cause and carrying the partial progress as a `*RunResult` (nil
  `FinalOutput`): input, generated items, raw responses, usage, guardrail
  results, diagnostics. It wraps UNCONDITIONALLY, whatever the cause's type.
  Errors from before the loop (bad options, unresolvable model) are returned
  bare.
- **`Classify(code, err)` tags an error without hiding it**: `errors.Is` and
  `errors.As` still reach the original. It is how `sandbox`, `mcp` and custom
  tools contribute a code.
- **The innermost classification wins.** `Classify` returns an already-coded
  error unchanged.
- **The code set is open.** An unrecognized code falls back to generic
  handling.
- **Errors a tool returns as text** (the sandbox file and patch tools) are
  model-facing results, not failures, and carry no code.
- **Submodule errors are classified at the module boundary**, not deep inside.
- **Recoverable failures are handled by error handlers in the loop, not as
  middleware** ([§2.12](#212-middleware)); a fallback message they synthesize
  is tagged `Source{Type: SourceErrorHandler}`.

#### How the safety valves compose

`ExecOptions` stacks `MaxTurns`, `ToolLoop`, `Overflow`, `ErrorHandlers` and
`ShouldStopAfterTurn` / `PrepareNextTurn`; their interactions are pinned:

- **Every recovered final output is still a final output.** A fallback from
  any `ErrorHandlers` handler and the output derived by `ShouldStopAfterTurn`
  finish through the same tail as a model-produced answer (`finishRun`):
  agent-end hook, output guardrails, persistence. There is no side door to
  "finished".
- **`ToolLoopError` has no handler.** A tripped tool-loop valve is always
  fatal.
- **An empty final turn retries before it fails.** With no `InvalidFinalOutput`
  handler (or one that declines), a structured-output turn that produced no
  text calls the model again.
- **Queued input outranks the finish.** A `Steer` that missed the save point,
  or a `FollowUp`, continues a run that had produced its final output INSTEAD
  of `finishRun`; `MaxTurns` still bounds the continued run.
- Overflow retries spend no turn budget and move the tool-loop valve neither
  way ([§2.5g](#25g-context-overflow)); only tool-calling turns count toward
  `MaxConsecutiveErrorTurns` ([§2.7d](#27d-tool-loop-safety-valves));
  `ShouldStopAfterTurn` is asked before `PrepareNextTurn`
  ([§2.3a](#23a-the-save-point)).

---

### 2.11 Event fan-out

One producer's events reach many independent consumers through `Fanout[T]`,
each subscriber with a buffer of its own.

- **Publishing never blocks on a consumer.** A subscriber that cannot keep up
  loses items rather than stalling the producer or its peers.
- **A dropped item is always reported.** The next delivery on that
  subscriber's stream is preceded by a `*GapError` naming the range it lost.
- **A cursor below the replay window reports the evicted range as a gap**
  running forward from the cursor (with no replay window at all, from the
  cursor to the head).
- **A cursor AHEAD of the head is a timeline reset**: the gap reports
  `LastGood` 0, the stale cursor as its `Dropped` count and the timeline's next
  sequence as `Next`, and is not `AtEnd` — so resubscribing from `LastGood`
  replays the new timeline from its start. It is delivered **immediately on
  subscribe**, not on the next publish.
- **A producer that finishes while a subscriber is behind reports the drops as
  the stream ends**: `GapError.AtEnd` true, `Next` zero and a zero-value item.
  Cancelling gets no such gap.
- **The zero item beside an `AtEnd` gap is not an event**; a consumer that
  forwards items onward skips it.
- **Sequence numbers are monotonic and assigned atomically with delivery**, so
  a subscriber never observes a higher number before a lower one, under
  concurrent publishers included.
- **A subscriber's replay backlog precedes anything published after it
  attached.** Registration and backlog delivery are one atomic step.
- **`Close` means "nothing more will be published"**, not "discard what you
  have": buffered items are still delivered, and a publish already accepted is
  waited for rather than lost.

— see [decisions §5.55](../explanation/decisions.md)

---

### 2.11b Run control

`Run` returns a `RunControl` alongside the stream, safe to use from another
goroutine, including before ranging begins. It is stop + injection + pending,
nothing more: a host renders progress from the stream's own events. Beyond
`StopAfterTurn`, three **injection methods** feed one arrival-ordered queue:

| | Consumed at | Extends a finishing run |
|---|---|---|
| `Steer` | the save point, or the final output | yes — it is "change course" |
| `NextTurn` | the save point only | no — it rides along with a turn the run was taking anyway |
| `FollowUp` | the final output | yes — the exchange lands, then the next one starts |

- **One queue, arrival order, across kinds.** The kinds are consumption
  filters, not separate queues.
- **Delivery is transactional.** Taking input moves it in flight; it counts as
  delivered once it lands in a persisted turn, in a serialized `RunState`'s
  item log, or — with no session — in an attempt that completed. A failed or
  abandoned attempt rolls its take back into the queue at its arrival
  position. A commit fires only against a home that holds the take: a session
  write that persisted PAST it, or a persist that succeeded at an interruption.
- **`RunState.PendingInput` seeds a resumed control once, before `ResumeRun`
  returns it** — never lazily when ranging begins.
- **A resume can keep the control it paused under.** `ResumeRunWith(ctx,
  state, opts, ctrl)` continues on the control `Run` returned and carries its
  live queue as is, never reseeded; `ResumeRun` mints a fresh control and
  seeds it
  ([§5.45](../explanation/decisions.md#545-a-middleware-resumes-under-the-callers-control)).
- **A follow-up continues the same run**: one trace, one usage total, one
  session.
- **Injected input becomes a run item** with `Source{Type: SourceUser}`,
  treated downstream exactly like the input the run started with. Its stream
  event is `injected_input_created`; `"unknown"` stays reserved for
  `ItemUnknown`.
- **Nothing is silently dropped.** `Pending()` reports what a run did not
  consume.
- **Queued input survives an interruption** on `RunState.PendingInput`, across
  serialization ([§2.1](#21-the-run-loop)). The wire shape is three lists and
  does not record cross-kind arrival order — an accepted loss at the pause
  boundary.

### 2.11e Span coverage

- **Typed spans cover**: agent, generation, function, handoff, guardrail,
  compaction, model retry, MCP, sandbox.
- **A retry span is a zero-duration marker**, not a wrapper: it records THAT
  an attempt failed, after the fact.
- **The current parent span travels on the `context.Context`** — the only
  channel a `Model` decorator, an MCP client or a sandbox backend receives.
- **`StartSpanFrom` returns a usable no-op handle without a trace**, so an
  instrumented call site never branches.
- **A finished span belongs to the processor.** `Set` and `SetError` after
  `Finish` are ignored, under the same lock the annotations take, so
  annotating from another goroutine is safe and simply drops. Writing through
  the exported `Span` field bypasses this and races, as mutating a `Trace`
  after `StartTrace` does.
- **A dropped span is announced through `BatchProcessorOptions.OnDrop`**; the
  processor's queue is bounded, and the SDK never writes to `slog.Default()`
  ([§2.11c](#211c-logging)). `Dropped()` remains the cumulative counter.
- **The runner installs the generation span as parent for the model call**
  (retries nest under it) **and the function span for a tool invocation** (MCP
  and sandbox work nests under the call that caused it).
- **A span carries the id that joins it to what it produced**: `call_id` on a
  function span, `response_id` on a generation span, recorded whether or not
  sensitive data is.
- **Sandbox is instrumented at the tool layer**, not per backend.

### 2.11d Diagnostics

A `Diagnostic` records trouble a run went through **and survived**: retries, a
fallback to a slower model, a compaction pass that gave up, a recovered tool
panic, a tool timeout converted into model-visible output.

- **They land on `RunResult.Diagnostics`** (on a failed run, through
  `RunError.Result`) **and on `session.Entry.Diagnostics`.**
- **Each is attached to the turn it happened in**, on that batch's last entry.
- **The sink travels on the `context.Context`**, like the span parent
  ([§2.11e](#211e-span-coverage)); `RecordDiagnostic` is a no-op without a
  sink.
- **`DiagnosticType` is an open vocabulary**: an unknown type is displayed
  generically, never rejected.

### 2.11c Logging

- **Off by default.** `LogConfig.Logger` is nil unless a caller sets it; the
  SDK never writes to `slog.Default()` on its own.
- **Sensitive data is a second, separate opt-in.** Attributes carrying
  conversation content are dropped unless `SensitiveData` is set; the record
  still appears without them. Outside that filter a `Sensitive` attribute
  renders as a redaction marker — its `LogValue` never reveals the value.
- **Every record carries `component`.**
- **The logger's handler sets the level floor.** Most of what the SDK says is
  `Debug`; there is no `Level` override field.
- **Logging and tracing are configured separately**, their sensitive-data
  switches included.

### 2.12 Middleware

`RunOptions.Middlewares` wraps a whole run, **outermost first**. A middleware
may edit the input and options, call `next` zero or more times, and replace or
suppress events — retrying, re-running with feedback, resuming from an
interruption.

**What is not middleware:** handoffs (they change which agent the state
machine is in), guardrails (they race the model call and can cancel it),
session persistence (a boundary only the loop knows,
[§2.5](#25-session-persistence-boundaries)), tracing (spans nest with the
loop's structure), `ExecOptions.ErrorHandlers` (needs the run's in-flight items
and the loop's completion path), `ModelOptions.InputFilter` (per turn, not per
run).

- **A middleware must not swallow the stream** — three clauses, stated on
  `RunMiddleware`'s godoc:
  1. Every event other than `RunCompletedEvent` flows through as it happens.
  2. `RunCompletedEvent` appears exactly once, last, on a run that ends without
     error and never on one that errors; a re-entering middleware holds back
     each attempt's and emits one for the attempt it accepts.
  3. Once the consumer stops ranging, nothing more is yielded, not even an error.
- **Order is behavior.** A middleware that resolves one attempt (an approval
  pause) sits inside one that decides whether to make another (an evaluator
  loop, a retry).
- **A stop the caller asked for is visible on the result**
  (`RunResult.StoppedEarly`), wherever the run ends — the turn boundary that
  saw it, or a final output reached on that same turn. It answers "did the
  caller stop this", not where; the stop is never cleared.
- **A middleware that resumes strips `Middlewares` first**: the chain is
  already unwound.
- **A middleware that resumes keeps the caller's control**: `RunInput.Control`
  is the handle `Run` returned, and an in-chain resume goes through
  `ResumeRunWith` with it
  ([§5.45](../explanation/decisions.md#545-a-middleware-resumes-under-the-callers-control)).
- **The session is the memory, not the input.** A re-entering middleware never
  re-sends what the session already holds
  ([§5.44](../explanation/decisions.md#544-middleware-and-sessions-the-session-is-the-memory-not-the-input)).
- **`Loop` carries only the evaluator's feedback when a `Session` is attached**
  (the whole attempt otherwise); **`Retry` re-runs with no input once an
  attempt announced its save** (`ItemsPersistedEvent`,
  [§2.5](#25-session-persistence-boundaries)) and re-sends the input otherwise.
- **The public `ResumeRun` applies `opts.Middlewares` exactly as `Run` does.**
  The paused state's agent and input are already decided; a middleware's edits
  to those fields do not apply on resume.

**Workflow middlewares (`Plan`, `Todo`)** rewrite the ENTRY agent only; handoff
targets keep their own toolset.

- **Plan gates by DENYING, not hiding.** While planning, a tool outside the
  read-only set stays in the model's toolset and answers a call with a refusal
  naming `submit_plan` — a normal tool OUTPUT, never an error. MCP tools are
  gated the same way, per turn.
- **Handoffs ARE hidden while planning** (`Handoff.IsEnabled`). The gate
  composes with the predicate it wraps, so unlocking never resurrects a
  handoff the host itself disabled.
- **A first-party tool's `ReadOnly` is trusted; an MCP tool's is not.** An MCP
  tool is admitted while planning only when the caller named it in
  `ReadOnlyTools` (`DefaultReadOnlyTools` when nil), never on the server's
  `readOnlyHint` alone. Nothing checks that a read-only tool behaves.
- **The refusal outranks approval.** A gated call needs NO approval while
  planning — not the tool's own predicate, and not the agent's `ApproveTools`
  listing, which `Apply` translates into per-tool predicates (MCP tools and
  `"*"` included) so the phase can suppress it. A read-only tool the listing
  names keeps its approval in BOTH phases.
- **`PlanPhase` is per RUN, and the unlock's scope is the host's.** `Apply`
  mints a fresh phase per run; a host wanting an approved plan to hold across
  later turns consults its own record and calls `Unlock` before the run.
- **`Plan.Apply` is safe to call unconditionally.** An unlocked phase gates no
  tool, offers no `submit_plan` and emits no preamble, so the same agent is
  rebuilt for a durable resume.
- **The plan review is an ordinary approval pause.** `submit_plan` is always
  approval-gated; approving unlocks and the SAME run continues, rejecting feeds
  the message back and planning continues.
- **`todo_write` replaces the whole list, atomically**; a malformed list is
  refused whole. An empty status defaults to pending. `todo_write` is on
  `DefaultReadOnlyTools`.
- **The rewrite is exported as `Apply`**, so a durable-resume host rebuilds
  WITH the plan/todo tools; `Plan.Apply` returns the run's `*PlanPhase`, and
  `Unlock` starts a rebuilt run in the executing phase.
- **A durable-resume host persists the UNLOCK, and persisting it is the
  unlock's precondition.** `PlanPhase.OnUnlock` fires once, when the approved
  `submit_plan` executes; its error fails the unlock and the phase stays
  planning (a submit_plan tool error; the model resubmits).

— see [decisions §5.53](../explanation/decisions.md)

---

### 2.13 Background tasks

A task is a sub-agent that outlives the turn that started it
([tasks.md](../howto/tasks.md)).

**Identity and transitions**

- **Identity and execution are separate.** `Task.ID` is the durable entity,
  `Task.RunID` one attempt at it.
- **Finalization is a compare-and-set**: status and result in one atomic
  transition, only while the task is non-terminal. A task store is
  transactional; no file-backed one is offered.
- **Every attempt-scoped write names its attempt.** `Finalize`, `Advance`,
  `ReleaseRetryClaim`, `MarkInputRequired` and `ReclaimWorking` carry a run id
  and lose when it is not the current one. A stale approval's write is a
  silent no-op, its resolve is refused as stale and discarded, and the expiry
  reaper finalizes against the approval's OWN run id. A stop chases **one**
  retry.
- **`input_required` is not terminal**, and a restart sweep leaves it alone.
- **A failed task can be retried in place** — `failed → working`, the only
  transition out of a terminal state, a compare-and-set that lands the new run
  id, the incremented attempt and the cleared summary and result together,
  only while failed and under the ceiling. **The ceiling is a store
  predicate**, so two processes cannot both claim the last attempt.
- **A launch that failed never counts as an attempt.** `ReleaseRetryClaim`,
  bound to the claimed run id, puts the task back to failed, rolls the attempt
  down (floored at 1) and records the launch failure as the result. Only the
  launch path releases; a run that registered and then failed is a real
  attempt.
- **A retry takes a concurrency slot** like a spawn.
- **Retryability is derived from state in hand.** `MaxAttempts` hands over the
  ceiling, and every consumer derives the offer from the status and attempt it
  already tracks. Capacity is excluded: it arrives as `ErrTaskLimit` at call
  time; a lost claim is `ErrRetryConflict`, a conflict to retry.

**Multi-run jobs**

- **A task is the ONE shape of background work, and its work may span several
  runs.** A step sequence or a loop is a task whose runs are chained; the
  hidden session, stop, retry, sweep, approval pause, cap and wake-up are
  written once.
- **`Config.Continue` is asked when a run of the current attempt completes or
  fails** — only for an outcome that NAMES the run, and only while the row is
  still working on it (a row paused for an approval is not moved on). Never
  for a cancellation nor a superseded attempt's outcome; an error from it, or a
  next run that fails to launch, ends the task failed.
- **A `Continuation` moves the task through `Store.Advance`**: run id and the
  host's `State` replaced in ONE compare-and-set, only while the task is
  working on the run the hook was asked about (nil `State` keeps the recorded
  one). `Advance` with the same run id on both sides rewrites `State` under
  the CAS.
- **A `Continuation` without an `Input` ends the task**, its final `State` in
  the same `Finalize` as the ending.
- **A transition the claim does not win is finalized on the run that ended,
  failed** — `Finalize`'s own predicate then decides.
- **The chain is bounded.** `Config.MaxContinuations` (default 50) further
  runs since the spawn or the last retry; a hook still asking at the bound
  ends the task failed.
- **`Task.Kind` and `Task.State` are the host's vocabulary and record, opaque
  to the SDK**; `Config.DescribeState` is how a host says where a job stands.
- **One cap governs every kind** (`MaxConcurrentPerParent`), **and one
  vocabulary for the model: four verbs** — `spawn_task`, `task_status`,
  `task_retry`, `task_stop`. A host with more kinds provides its own spawn
  tool from the public parts (`SpawnTool` / `TaskTools`, `Spawn`,
  `ModelHasResult`, `ToolResult`) with the kind as a parameter, never a fifth
  verb.

**Endings, stops and delivery**

- **The Manager REPORTS endings; it does not deliver them.** A terminal
  transition it claimed calls `Config.OnFinished`; a result the model pulled
  in-turn calls `Config.OnResultDelivered`. When a session may be interrupted,
  and holding the debt until then, is host policy.
- **A cancellation is reported as DELIVERED, not finished.**
- **The reported `*Task` is the claimed snapshot in hand**, built from the
  finalize's own values, never a re-read.
- **A durable host writes its own debt row ATOMICALLY with `Store.Finalize`**
  (and `ReleaseRetryClaim`), not from the hook; drops an undelivered debt
  inside `RetryClaim`'s transition; and writes each orphan's debt in the
  restart sweep's own transaction.
- **A paused task is claimed before the host is told to stop it**; a working
  one has its run cancelled first.
- **A stop reports what it DID**, and the four answers are not
  interchangeable. **`StopAfterTurn` is the only answer that ends the call.**
  `StopAlreadyFinished` claims nothing, writes no cancellation, and sends the
  stop round again.
- **An outcome that is late is waited for; one that is lost is replaced.** The
  stop waits, briefly and boundedly, for the ending before its last pass, and
  records a cancellation only if none arrives. A host answers
  `StopAlreadyFinished` only once the run's own recording has had its chance.
- **Whether a run reported is the Manager's own knowledge, not the row's.**
  Compensation looks at the row AND whether `OnRunFinished` has spoken about
  that run.
- **Starting a run is two steps, and a terminator can land between them.** A
  stop tells the host AGAIN once the ending is unambiguously its own, and
  `Spawn`/`Retry` re-read the row after launching — if it no longer names
  their run as its live attempt, they cancel the run they started and report
  what the task actually is.
- **A result counts as delivered on the MODEL's path only, and only for the
  result the model is actually handed.** A task that ends before the call
  that started it returns is delivered (`OnResultDelivered`); one still
  reported as running is NOT, however the row reads by then; the attempt is
  checked.
- **`task_retry` answers every call that has task state with that state**, so
  a launch failure it reports counts as delivered. **A host API never reports
  delivery.**
- **The restart sweep runs BEFORE the host accepts requests**, as a separate
  call from whatever delivers; `FailOrphans` RETURNS the rows it failed. Two
  processes sharing one store keep the race.

**Sessions and safety**

- **A task row names sessions by GENERATION, not by id**
  ([§2.5e2](#25e2-the-entry-lifecycle-contract)). A store binds the parent's
  and the child's generation on write, and every by-session read compares
  them against the generation answering to that id NOW.
- **A `session.Repo` that owns both tables deletes the task TREE with the
  session**: the rows in both roles and the hidden sessions, at any depth.
  *Per backend*; the Manager deletes nothing, and a repo without a task table
  leaves the rows and child sessions to the host.
- **A task carries the configuration snapshotted at spawn** (`Task.Inherit`)
  and the run that spawned it (`Task.ParentRunID`), so the delivering turn
  runs as the agent that asked and the relationship lands on the run's traces.
- **Rollback of a half-finished spawn uses a detached context.**
- **A depth check that cannot be made refuses**: `MetaFor` reports a failed
  lookup rather than resolving it to "not a task".
- **A notification is a user-role entry** the model reads verbatim; a UI
  renders it as a card. **Its line is machine-readable and its fields come
  from untrusted text**: formatting escapes the line delimiter AND the field
  delimiter, and the retry hint is its own line. Formatting and parsing ship
  together.
- Defaults: depth 1 (a task cannot spawn tasks), 6 concurrent tasks per
  parent, 300-rune summaries, a 120s bound on `task_status`'s wait, 3 attempts
  per task.

— see [decisions §5.54](../explanation/decisions.md)

### 2.14 The SDK reads no environment variable

Everything the SDK acts on is passed in: `RunOptions`, the `Agent`, its
`ModelSettings`, and the provider/backend constructor options.

- **The `agents` package calls no `os.Getenv`.**
  `rg 'os\.(Getenv|LookupEnv)' agents/` is the guard and must stay empty.
- **A wrapped vendor client library keeps its own env contract.** openai-go
  defaults its key from `OPENAI_API_KEY` when no `option.WithAPIKey` is
  passed; the caller opts in by constructing the provider without a key.
- **An OS-integration backend may consult the standard variables of the tool
  it drives.** The docker sandbox honors `DOCKER_HOST` and its siblings (and
  `SSH_AUTH_SOCK` for an `ssh://` daemon); the local sandbox passes `PATH`,
  `HOME` and `TMPDIR` through to the child. Each is documented on the backend
  and overridable by an explicit option.
- **`Observe.IncludeSensitiveData` nil means include** ([§4](#4-reference-behavior-you-can-rely-on));
  no variable decides it.

— see [decisions §5.39](../explanation/decisions.md#539-the-sdk-reads-no-environment-variable-of-its-own)

### 2.15 The model adapter contract

Every `agents.Model` implementation satisfies the runner's consumption
contract, enforced by `modelkit/conformancetest`; both in-repo providers run
it.

- **Output items are canonical Responses items whose `RawJSON()` is non-empty
  wire JSON** — `agents.OutputToInput` and session persistence depend on it.
  The runner models `message` / `reasoning` / `function_call`; anything else
  rides through as an `ItemUnknown` run item.
- **The stream vocabulary is `response.*` only.** The first event is
  `response.created`; each finished item gets one `response.output_item.done`,
  in order; the terminal event is `response.completed` or
  `response.incomplete` (reason `max_output_tokens` is the one recoverable
  truncation, [§2.7e](#27e-truncated-responses)).
- **Text streams as `response.output_text.delta`, raw reasoning text as
  `response.reasoning_text.delta`.**
- **The event names are spelled ONCE, as the exported `agents.Event*`
  constants**; the runner's classifiers, `modelkit`'s constructors, the OpenAI
  adapter's terminal-event switch and `conformancetest`'s closed set build
  from that list. `agents/stream_events_test.go` alone restates the wire
  strings by hand.
- **`response.queued` is tolerated wherever `response.created` /
  `response.in_progress` appear, but only a pass-through backend emits it**;
  `modelkit` offers no constructor for it and the conformance closed set
  leaves it out.
- **Usage is Responses semantics**: `InputTokens` is the TOTAL input count,
  cache reads and writes included; `CachedTokens` / `CacheWriteTokens` are
  informational subsets.
- **Unsupported request features fail loudly** — a `*agents.UserError` naming
  the feature (`modelkit.Reject`), never a silently dropped setting.
- **Continuity blobs (thinking signatures, redacted reasoning) ride in the
  reasoning item's `encrypted_content`**, the one slot that survives
  `OutputToInput` and session storage. A reasoning item without one is dropped
  on replay to a backend that requires signatures.
- **A backend that reports overflow in a success-shaped response surfaces it
  as an error carrying the overflow marker**
  ([§2.5g](#25g-context-overflow)).

— see [decisions §5.10](../explanation/decisions.md#510-non-responses-backends-adapt-at-the-model-boundary);
the Anthropic mappings are in [howto/models.md](../howto/models.md)

### 2.16 MCP client: shared connections and retry

An MCP session is shared by everyone configured with that server — several
runs, their background tasks, other conversations.

- **A request on a shared connection rides the connection's context, not the
  caller's.** A caller's cancellation is honored by returning from the wait;
  the request itself is not cancelled, and its late answer is dropped.
- **An in-flight request is bounded by the connection's lifetime (`Close`)
  and a request ceiling of 30 minutes.**
- **A resource shared between runs is never handed a single run's
  cancellation.**
- **A dead connection repairs itself in place** when given
  `mcp.Options.Redial`, so every holder of that server recovers. Without
  `Redial` the failure is reported, not repaired.
- **A death is noticed as it happens** — the connection is watched — **and
  healing is throttled.**
- **Only idempotent work is repeated.** `tools/list` is re-issued on the fresh
  session; a failed tool CALL is reported to the model, never retried by the
  redial.
- **`MaxRetryAttempts` retries on transport failure only.** An answer the
  server SENT — JSON-RPC parse error, invalid request, unknown method, invalid
  params, the transport's own "rejected" — is not retried, nor is a call made
  after `Close`. Each attempt reloads the session.
- **The delay doubles per attempt, capped at 30s, jittered into `[d/2, d]`.**
  `-1` means one attempt every 30s until the caller's context ends.
- **The MCP client does not share the model layer's `RetryPolicy`**; the
  timing matches, the classification does not.

Model-side retry, the counterpart rule:

- **The SDK's one retry layer for models is `agents.NewRetryModel`.**
  `openai.NewProvider` and `anthropic.NewProvider` build their clients with
  `WithMaxRetries(0)`; a provider used without `NewRetryModel` performs no
  retries, and a caller's own `option.WithMaxRetries` is appended after the
  default and overrides it.
- **A `Retry-After` longer than `MaxDelay` ends the retries**, returning that
  attempt's wrapped error rather than clamping to the cap and trying again.

— see decisions
[§5.20](../explanation/decisions.md#520-a-shared-connection-is-not-a-callers-to-cancel),
[§5.21](../explanation/decisions.md#521-a-dead-shared-connection-repairs-itself-and-a-tool-call-is-not-repeated),
[§5.21b](../explanation/decisions.md#521b-an-mcp-retry-waits-on-the-transport-never-on-an-answer),
[§5.22](../explanation/decisions.md#522-retry-policy-lives-in-one-layer)

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

§5.5 makes the Responses wire types the canonical format, and §5.5b accepts
that they therefore appear in nearly every exported signature by way of
`InputItem` and friends. What neither records is the consequence for §5.8:
**an `openai-go` v3 → v4 bump is transitively breaking for every downstream
package**, since their signatures name those aliased types.

That is survivable before v1.0.0, where §5.8 allows breaking minors. After it,
a v1 that promises compatibility is implicitly promising `openai-go/v3`. Three
answers, none chosen yet:

- **Take the bump as agents-go v2.** Honest, and expensive for an ecosystem
  that has to move in lockstep with a dependency's schedule.
- **Fork the item types.** Buys independence at exactly the cost §5.5b
  declined: a conversion layer chasing every Responses API addition forever.
- **Stay on v3 for the life of v1.** Cheapest until a provider feature only
  the new major exposes.

Whichever is taken, it belongs in §5 before v1.0.0 is tagged.

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
2. An entry is bullets of invariants, ≤3 lines each; a sentence that explains
   *why* is evicted to [decisions](../explanation/decisions.md). Rationale
   never lives here.
3. A new §6 entry does not need an immediate answer, but the PR that implements
   it must move it out of §6 first.
4. A decision — the reasoning, not the resulting behavior — is recorded in
   [design decisions](../explanation/decisions.md) under a new §5 number.
   Numbers are never reused: code comments cite them as `decisions §5.29`.
5. Upstream changes are tracked in [upstream_watch.md](../explanation/upstream_watch.md) with
   **no obligation to match**.
6. Users migrating from the Python SDK: see
   [migration_from_python.md](../explanation/migration_from_python.md).
