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

### 2.3a The save point ✅

The **save point** is the turn boundary: the turn's assistant message and every
tool result are persisted, and the next model call has not happened yet.

It is one place in the code, and its step order is the contract:

1. flush the turn to the session
2. ask `ShouldStopAfterTurn`
3. compact ([§2.5f](#25f-compaction-)), rebuilding the context from the log
4. call `PrepareNextTurn`

Persisting first is what makes the rest safe: a run that stops at step 2, or
whose context is rewritten at step 3, has its history already written. Asking
to stop before compacting means the decision is made against the turn that
actually happened rather than a shortened view of it.

A **handoff** reaches only step 1 and 2. The next turn belongs to a different
agent, so its snapshot is resolved fresh, and its context is about to be
rewritten by the handoff input filter.

### 2.3b Turn snapshots ✅

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

### 2.3c Stopping early ✅

A turn that would otherwise continue can be ended from two places, and only
two:

| Level | Mechanism | Final output |
|---|---|---|
| tool | `ToolResult.Terminate` | the last tool's output |
| run | `ExecOptions.ShouldStopAfterTurn` | the turn's last message, else its last tool output |

- `Terminate` requires **unanimity** across the batch ([§2.7b](#27b-tool-results-)).
- `ShouldStopAfterTurn` is consulted at the **save point** ([§2.3a](#23a-the-save-point-)), at both branches that would take another turn, including a handoff. A run stopped there has its full
  history saved and needs no unwinding, and stopping at a handoff means control
  never leaves the agent.
- It is **not** consulted on a turn that already ends the run: asking whether to
  stop something that is stopping is noise.
- It is a **predicate, not a producer**. The final output is derived from the
  turn, so a stopped run's result cannot disagree with its saved history. A
  caller wanting something else computes it from `RunResult.NewItems`.
- Both survive `ResumeRun`: an approved run carries the same stop policy, or it
  would sail past the point it was configured to stop at.

There is deliberately no agent-level early-stop configuration. Naming tools up
front cannot express anything the turn predicate cannot, and the policy belongs
to the run — the same agent gets reused across runs that stop at different
points.

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

### 2.5b Session entries ✅

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
- **A checkpoint is self-contained** — it carries the retained tail inside it,
  so reading it gives the whole context that replaced the folded history.

**Entries are append-only.** ✅ Nothing is rewritten in place; that is what lets
a session be forked, shared and read concurrently without a writer invalidating
a reader's view. A display settled after its turn ended is expressed as an
**update entry** naming its target, folded in at read time:

- Updates apply in stored order; the last write wins per field.
- **An update may be stored before its target.** Association is by id, so the
  "the task finished before the parent turn was persisted" race does not need
  handling — it cannot occur.
- **An update whose target is missing is ignored, not an error.** The target may
  have been folded away by compaction, and failing an entire read over a stale
  pointer would make history unloadable.

A server-managed conversation (`openai.ConversationsSession`) can hold only
items; other kinds are dropped on write, because failing a run over a UI
annotation that could not be stored server-side is worse than losing it.

### 2.5c Session layering ✅

A session is three layers, split along what varies:

- **`SessionStorage`** reads and writes entries and understands nothing about
  what they mean.
- **`Session`** is a concrete type, not an interface, that turns entries into
  the model's view. Storage varies; "how history becomes model input" does not,
  and as an interface every backend re-answered it and they drifted.
- **`SessionRepo`** owns lifecycles — create, open, list, delete.

**Reads page on sequence numbers, not offsets.** ✅ Entries keep arriving, so an
offset shifts under a concurrent append and a second page silently skips or
repeats. A negative `Cursor.Limit` takes the most recent N.

**Derived state is a fold, never a stored field.** ✅ `State` and `Stats`
recompute from the entries. A field maintained beside the log has to be updated
on every write and can disagree with it after a crash, a concurrent writer or a
fork; a fold cannot.

**`ContextEntries` starts at the most recent compaction checkpoint** ✅ — the
checkpoint stands in for everything before it, and re-sending that history would
undo the compaction.

Capabilities a store may or may not have are **optional interfaces**, not
required methods: `AtomicReplacer`, `EntryPopper`, `CompactionAware`. Popping in
particular is not in `SessionStorage` because a run never pops; requiring it
would tax stores that cannot (a server-managed conversation) for a feature the
run loop does not use.

### 2.5d Sessions are trees ✅

An entry names its parent, so a session is a walk rather than a pile.

- **Branching abandons without deleting.** ✅ Moving the active branch leaves the
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
- **A walk stops at a compaction checkpoint** and at a missing parent (an
  ancestor may have been folded away). A repeated id also stops it, so a corrupt
  session reads short instead of hanging.

**Fork extracts a branch; branch moves within one session.** ✅ A fork carries
entry ids across unchanged, so an update entry naming one still finds its
target.

### 2.5e Session lifecycles ✅

A `SessionRepo` owns which sessions exist, separately from their contents.

- **Existence is recorded, not inferred.** A session created with no entries is
  still listable; inferring existence from contents makes "empty" and "never
  created" the same state.
- **`Hidden` belongs to the session**, not to each caller's filter. A session
  that serves another one (a background task's history) is excluded from
  listings by default.
- **Opening an unknown session is an error**, never an empty session. A wrong id
  that reads as a fresh conversation makes a run start over instead of
  continuing, which is worse than failing.
- **Deleting removes the entries with it**, atomically where the backend can, so
  no entries survive pointing at a session that is gone.

### 2.5f Compaction ✅

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
- **Local compaction and server-held history do not interact**, because
  `UsePreviousResponseID` / `ConversationID` already refuse a local `Session` —
  there are no local entries for a compactor to see.
- A **self-compacting storage** (`CompactionAware`, e.g. the server-side
  compact API) takes the `CompactAfterRun` point instead; the two never both
  run on one session.

**A checkpoint is appended, never a rewrite.** `CompactAfterRun` records the
pass as an `EntryKindCompaction` entry whose payload names the entries it
folded (`ExcludedIDs`) and carries the retained tail. The folded entries stay
in the session untouched, so a reader can offer to expand them and a fork from
before the checkpoint still finds its full history; `ContextEntries` starts at
the most recent checkpoint, so the next run reads the shorter context without
recomputing the pass.

Writing a checkpoint is an optional capability (`CompactionCheckpointer`): a
compactor that only reshapes the context in memory is useful and has nothing
durable to say.

The one path that still rewrites is `openai.CompactionSession`, because the
server's compact API returns a replacement rather than a decision.

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
  For finer rewriting use `ModelOptions.InputFilter`, which edits the exact
  items sent without changing what is saved.
- `output` — replaces the final output value.
- `tool_input` — the tool does not execute; `Message` becomes its result.
- `tool_output` — replaces the content returned to the model.

🚧 (plan3) streaming currently runs input guardrails synchronously rather than
concurrently. Unifying the streaming and blocking APIs removes this difference;
the target is one behavior — concurrent, with cancellation.

### 2.7 Tools

#### Return values ✅

A tool returns a `ToolResult` ([§2.7b](#27b-tool-results-)); plain values are
wrapped. What the model sees, given the result's `Content`:

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

### 2.7b Tool results ✅

A tool returns a `ToolResult`, not a bare value. The distinction it makes is
that some of what a tool knows is **not for the model**:

- `Content` reaches the model. `Details` never does — it lands on the item's
  `Display().Extra`, for the UI and for logs.
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

### 2.7c Tool capabilities are side interfaces ✅

Beyond its name, everything a tool can do is an **optional interface** rather
than a method on `Tool`: `InvokableTool`, `DescribableTool`,
`ApprovalRequiredTool`, `GuardedTool`, `TimeoutTool`, `SequentialTool`,
`EnableableTool`, `FailureHandlingTool`.

- The runner resolves them **only** through `ToolAs[T](tool)`, which walks the
  `Unwrap() Tool` chain the way `errors.As` walks an error chain. A tool that
  provides nothing beyond `Tool` is legal and runs with every default.
- A **bare type assertion is a bug**, and the reason this is specified at all:
  `tool.(ApprovalRequiredTool)` compiles and returns false through any wrapper,
  silently reporting that a tool needing approval needs none. Nothing in the
  type system catches it, so the rule is "always `ToolAs`".
- `WithApproval`, `WithTimeout`, `WithGuardrails`, `WithEnabled`,
  `WithSequential` and `WithFailureHandler` wrap any tool. They stack in **any
  order**, and every capability underneath stays reachable.
- `WithGuardrails` **appends** to the wrapped tool's own guardrails. Replacing
  them would let a wrapper disarm a tool's checks without saying so.
- A wrapper never changes what the model is told: name, description and schema
  come from the tool underneath.

There is deliberately no `WithFatalErrors`. "Errors abort the run" is the
absence of a failure handler, and a wrapper cannot express an absence —
`ToolAs` would walk straight past it to the inner tool's handler. Set
`FailureErrorFunction = nil` on the tool itself.

### 2.7d Tool-loop safety valves ✅

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

### 2.7e Truncated responses ✅

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
- Both model paths report it. The blocking path used to drop `Status` while the
  streaming path read it, so the same response was a hard failure when streamed
  and a silent partial answer when not.

### 2.7f Usage attribution ✅

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
- **Nested usage is separate.** `SessionEntry.NestedUsage` and
  `ToolCallOutputItem.NestedUsage` hold what a tool spent on model calls of its
  own. It is not merged into `Usage`, because the two answer different
  questions: a nested run's tokens were spent on a different conversation, and
  counting them as context would make this one look larger than anything ever
  sent. It IS part of the run total, since the nested run shares the parent's
  usage.
- `RunResult.UsageByResponse()` and `RunResult.NestedUsage()` read it back:
  where the tokens went, and how many were spent off this conversation.

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

### 2.10 Errors and recovery ✅

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
  invalid final output), **in the loop, not as middleware** ✅ — see
  [§2.12](#212-middleware-).
- A fallback message synthesized by a recovery handler is tagged
  `Source{Type: SourceErrorHandler}`. ✅

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

### 2.11b Run control ✅

`Run` returns a `RunControl` alongside the stream. It is safe to use from
another goroutine, including before ranging begins.

Beyond `StopAfterTurn`, `Phase`, `CurrentAgent` and `CurrentTurn`, it has three
**injection queues**. They are separate queues rather than one with a mode tag
because they are consumed at different points and only two of them may extend a
run that was ending:

| | Consumed at | Extends a finishing run |
|---|---|---|
| `Steer` | the save point, or the final output | yes — it is "change course" |
| `NextTurn` | the save point only | no — it rides along with a turn the run was taking anyway |
| `FollowUp` | the final output | yes — the exchange lands, then the next one starts |

- **A follow-up continues the same run**, rather than starting a new one, so
  the trace, the usage total and the session stay one thing.
- **Injected input becomes a run item** with `Source{Type: SourceUser}`. That
  is what makes every downstream path — the next turn's model input, the
  server-side delta cursor, the session write — treat it exactly like the input
  the run started with, instead of each one having to learn about a separate
  pending-input list.
- **Nothing is silently dropped.** `Pending()` reports what a run did not
  consume, which is how a caller learns a `NextTurn` arrived too late.
- **Queued input survives an interruption**: `RunState.PendingInput` carries
  it, so a steer sent while a human was deciding on an approval is delivered on
  resume. That is precisely when someone is looking at the run and saying
  something about it.

### 2.11e Span coverage ✅

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
- The runner installs the generation span as parent for the model call (retries
  nest under it) and the function span for a tool invocation (MCP and sandbox
  work nests under the call that caused it).
- Sandbox is instrumented at the **tool** layer, the one place every backend
  (local, Docker, SSH) is reached through, rather than per backend.
- OTel semantic conventions are **pinned** (`SemConvVersion`); the GenAI
  conventions are experimental upstream and have renamed keys between releases,
  so a change there is a deliberate edit rather than a dependency-bump side
  effect. Spans with no GenAI equivalent use an `agents.` prefix — naming them
  `gen_ai.*` would imply a portability that is not there.

### 2.11d Diagnostics ✅

A `Diagnostic` records trouble a run went through **and survived**.

- The failures worth recording are the ones that do *not* fail the run: three
  retries, a fallback to a slower model, a compaction pass that gave up, a
  recovered tool panic. None of them reach an error return, so without this
  they live only in a log nobody kept, and "why was that answer bad" becomes
  unanswerable after the fact.
- They land on `RunResult.Diagnostics`, on `RunErrorDetails.Diagnostics` when
  the run does fail (the error is the last straw; the diagnostics are what led
  to it), and on `SessionEntry.Diagnostics`.
- **Each is attached to the turn it happened in**, on that batch's last entry,
  not repeated on every turn after.
- **The sink travels on the `context.Context`**, because a `Model` receives one
  and nothing else that belongs to the run. A sink passed by field would need
  every decorator in the chain to forward it, and the one that forgot would
  swallow silently. `RecordDiagnostic` is a no-op without a sink, so a
  decorator used outside a run still works.
- `DiagnosticType` is an **open vocabulary**: an unknown type is displayed
  generically, never rejected.

### 2.11c Logging ✅

- **Off by default.** `LogConfig.Logger` is nil unless a caller sets it; the SDK
  never writes to `slog.Default()` on its own. A library that logs the moment it
  is imported appears uninvited in somebody's production output.
- **Sensitive data is a second, separate opt-in.** Attributes carrying
  conversation content are marked and dropped unless `SensitiveData` is set;
  the record still appears without them. "Log what the SDK is doing" and "log
  what the user said" are different decisions, and only one of them puts a
  conversation into a log aggregator.
- **Every record carries `component`**, so SDK chatter is filterable by origin
  without each call site repeating the attribute.
- `Level` overrides the minimum level **for SDK records only**. Most of what the
  SDK says is `Debug`, and a caller usually wants it without enabling `Debug`
  application-wide.
- Logging and tracing are configured separately, as are their sensitive-data
  switches: exporting spans and writing log lines are different exposures.

### 2.12 Middleware ✅

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
| Session persistence | Has a boundary only the loop knows ([§2.5](#25-session-persistence-boundaries-)) |
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

**Order is behavior.** A middleware that resolves something about *one attempt*
(answering an approval pause) must sit inside one that decides whether to make
*another attempt* (an evaluator loop, a retry). Reversed, the outer one judges
a result the inner one had not finished producing.

**A middleware that resumes strips `Middlewares` first.** The chain is already
unwound at that point; resuming with the run's own options would re-enter that
middleware and every one outside it.

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

Sealed is not the same as unextensible: a caller cannot invent a tool *kind*,
but the `WithXxx` decorators of [§2.7c](#27c-tool-capabilities-are-side-interfaces-)
compose freely over any tool, so behavior stays open while the wire contract
stays closed.

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
