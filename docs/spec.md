# Design Spec

This is the behavioral specification for this SDK. Behavior questions are
answered here, not by [openai-agents-python](https://github.com/openai/openai-agents-python).

**The rule:** when this document does not cover a case, decide, implement it,
and add the invariant here **in the same change**.

Status markers used below:

- ✅ implemented and stable
- 🚧 specified but not implemented yet
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
| **Chat Completions API** | Internal item types *are* Responses types ([§5.5](#55-internal-item-types-are-responses-wire-types)). A backend that speaks another protocol is supported by translating at the model boundary ([§5.10](#510-non-responses-backends-adapt-at-the-model-boundary)) — never by making a second format canonical. Chat Completions specifically was declined again 2026-07-31 in favor of a native Anthropic adapter; revisit only with a concrete backend nothing else covers. |
| **Provider-hosted tools** (`web_search`, `file_search`, `code_interpreter`, `computer_use`, …) | A tool is a `*Tool` struct, not an interface, so there is nothing a hosted tool could implement; every tool executes locally. Hosted tools bind a tool to one backend. |
| **A neutral multi-provider abstraction** | No lowest-common-denominator message model. An adapter implements `Model` by translating to the canonical Responses format ([§5.10](#510-non-responses-backends-adapt-at-the-model-boundary)); `models/modelkit` is shared plumbing for writing adapters, not an abstraction layer. The SDK guarantees depth of correctness for Responses semantics. |
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
  The moment `emit` observes the departure it cancels the RUN'S OWN context
  root, so everything the turn has in flight — the model call, the tool batch
  the loop is blocked on, racing input guardrails — is told to stop rather
  than completing into a run nobody reads. Waiting instead parked the
  consumer's `break` for the work's full duration, and forever for a guardrail
  that returns only when cancelled; a tool that ignores its context still runs
  to completion, but its result is discarded. (One narrow exception to "the
  consumer's goroutine": a tool streaming progress yields from its own
  goroutine — [§2.7g](#27g-tool-progress-).)
- **A `RunStream` is single-use.** Ranging it a second time yields a
  `*UserError` instead of anything else: the run body lives inside the
  iterator, so a second range would re-execute it — model billed again, tools
  re-running their side effects, the session taking duplicates — and it would
  do so silently, which is how "break out early, then Collect()" once
  duplicated a run.
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
    check budget (turns; 🚧 tokens / deadline)
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
2. Budget exhausted → with `ToolLoop.FinalTurnWithoutTools`, call the model
   once more **without tools** so it can close out in prose. Otherwise return
   `*MaxTurnsError`. ✅
3. HITL interruption → return a `RunResult` carrying `Interruptions` and `State`.
4. The model produced a final output → see [§2.3](#23-deciding-the-final-output-).

**A `RunState` round-trips whole.** ✅ Everything a resume consumes is in the
wire format — the pending injected input, the disclosed deferred tools, the
server-conversation cursor included — pinned by a full-field round-trip test
(`RunStateSchemaVersion` 1.4). The in-process resume passing the live pointer
must never be the only path that works; the serialized surface IS the
contract. The cursor in particular rides along so a resumed run keeps sending
deltas: the resumed turn re-processes a response the restored cursor already
accounts for and does not advance it — re-deriving the cursor there marked
pre-pause sibling tool outputs as already served, and a server-managed
conversation never received them.

### 2.1b Items ✅

**`RunItem` is one struct with a `Kind`, not an interface.** The kinds are a
closed set the runner produces — message, tool call, tool output, handoff
call/output, reasoning, injected input, unknown — and a caller cannot add one,
which is the definition of a union, not of a polymorphic seam. As an interface
it took seven near-identical implementations (five were `{Agent, Raw}` plus a
tag) restating six methods each, and serialization still had to flatten them:
a stored `RunState` holds `{type, agent, input, source, display}`, and reading
it back required an eighth, unexported implementation whose only job was to
carry those fields. The struct IS that shape, live and stored.

Consumers switch on `Kind` and must treat an unrecognized kind as opaque —
render it via `Display()`, never fail — so the set can grow without breaking
them.

Beyond its payload, every item reports two things:

- **`Source` — who produced it.** The zero value is the model.
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
provenance and renders the same timeline as before the pause. A rebuilt item
carries its replayed input form (`RawInput`) and stored display; `Raw` is nil —
a resume replays history from input items, which is all it needs.

**An unknown output item is kept, never dropped.** ✅ A model output type this
SDK does not model becomes an `ItemUnknown` run item carrying the original bytes,
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
| 4 | **If any call needs approval, pause the whole turn — no tool runs** | Pausing only the gated calls would leave `RunState` holding partial results |
| 5 | Run `toRun` concurrently, then merge with `rejected` in **original call order** | Result order is deterministic and independent of completion order |
| 6 | A nested agent-as-tool interruption pauses the parent run too | Completed siblings **keep** their outputs; the interrupted call's output is **withheld** |
| 7 | Unknown tool → feed back `Tool 'X' not found.` | Only under `ToolNotFoundReturnToModel`; otherwise it is a `*ModelBehaviorError` |
| 8 | **Handoffs win**: switch to the target agent, end the turn | Tools in the same response have **already executed**; the final-output check is skipped |
| 9 | Decide the final output ([§2.3](#23-deciding-the-final-output-)) | — |

**Concurrency guarantees:**

- Tool concurrency is capped by `MaxToolConcurrency` (0 = unlimited). ✅
- A panicking tool is recovered and routed through that tool's error path. ✅
- When several tools fail, the error surfaced to the run is the one with the
  **lowest call index among non-cancellation errors** — never whichever
  goroutine finished first, and never a sibling's `context.Canceled` echo of
  the failure that cancelled it. Cancellation surfaces only when it is all
  there is (the consumer abandoned the run mid-batch). ✅
- A tool declaring `SequentialTool` forces the whole batch to run serially. ✅

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

This guarantee does not survive an abnormal process exit; a `RecoveryPolicy`
repairs dangling state when the session is reopened. ✅

**Entries are append-only.** ✅ An entry's display may need
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
- **A checkpoint copies nothing.** It NAMES what it folded (`ExcludedIDs`) and
  carries only content that exists nowhere else — the summary, and stand-ins
  for folded groups (`CompactionFold`). The entries a pass kept are never
  inside it: a copy of a live entry has to be kept in step with the tree, and
  the copy that fell out of step (a popped entry living on in the checkpoint
  that "retained" it) is why the earlier self-contained shape was removed.

**Entries are append-only.** ✅ Nothing is rewritten in place; that is what lets
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
- **Folding is a projection: it never writes through to storage.** ✅ Readers
  get shallow copies whose `Display` (and its `Extra` map) is shared with the
  stored entry, so the fold copies what it merges instead of editing in
  place — a read must never change what the next read returns.

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
fork; a fold cannot. `State` folds the ACTIVE BRANCH — the view recovery reads
— not append order: a dangling call on an abandoned attempt is not pending,
and folding every branch reported it forever as a stuck approval nothing could
clear. `Stats` stays whole-log, because it counts what is stored.

**`ContextEntries` is the active branch minus what compaction folded** ✅ — the
checkpoints themselves stay in the view (they carry the summary and stand-ins
the projection renders), while the entries their exclusions name are left out:
re-sending folded history would undo the compaction, and a cursor limit must
count entries the model will actually see. `ProjectEntries` applies the same
exclusions again wherever it is called, so a view built without the filter
still cannot replay folded history.

Capabilities a store may or may not have are **optional interfaces**, not
required methods: `AtomicReplacer`, `EntryPopper`, `CompactionAware`. Popping in
particular is not in `SessionStorage` because a run never pops; requiring it
would tax stores that cannot (a server-managed conversation) for a feature the
run loop does not use. **A wrapper that claims a capability delivers its
contract or refuses**: delegating `AtomicReplacer` to a wrapped store without
it must return an error before touching anything, never degrade to a
non-atomic Clear+Append — a caller type-asserted the interface precisely to
rule that failure mode out.

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
- **A linkless run of entries is one straight line, and the branch continues
  through it.** Entries carrying no parent ids and no leaf moves — a session
  from before branching existed, a server-held store with no tree — cannot be
  off-branch, since branching needs links. The walk to the leaf is extended
  over the linkless prefix ahead of its root, so the first LINKED append to
  such a session does not drop everything written before it. Every
  branch-scoped view (`ContextEntries`, `PathEntries`, the pop selection)
  shares this rule through one helper (`ActiveBranchOf`).
- **A walk does NOT stop at a compaction checkpoint.** The walk answers "which
  entries are on this branch", and a folded entry is still on the branch it was
  written to; what the model sees is projection's question. Stopping the walk
  at a checkpoint is how the kept entries once became unreachable to a pop
  while the model could still see them. A missing parent ends the walk (a
  filtered view may have dropped an ancestor), and a repeated id does too, so a
  corrupt session reads short instead of hanging.

**Fork extracts a branch; branch moves within one session.** ✅ A fork carries
entry ids across unchanged, so an update entry naming one still finds its
target. The destination is written through `ReplaceStorageEntries`, so a
storage that can swap atomically (`AtomicReplacer`) never shows a
cleared-but-unfilled fork target when a failure lands mid-write.

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
- **A name that two ids share belongs to whichever id claimed it.** A backend
  that maps ids onto a namespace with fewer characters than ids have — a
  filesystem — must record the original id and refuse to open or delete through
  the other one. **A check that cannot be made refuses**: not being able to read
  who owns the name is a reason to stop, not to proceed.
- **A failed create leaves nothing behind.** A backend that claims a name before
  it can finish writing gives the claim back if the rest fails, or the id is
  burned: unusable and un-recreatable.

### 2.5e2 The entry lifecycle contract ✅

Everything above describes what a session *is*. This describes what happens to
an entry over its life — minted, addressed, walked, removed — and it exists as
one section because the alternative was tried: these rules were decided one at a
time, in whichever backend a defect was reported against, and four
implementations drifted apart on every one of them.

**The rule this section is really about: none of it is a backend's decision.**
Each item below names who implements it. Where that is "shared", a backend that
answers the question itself is a bug even if its answer is right, because the
next backend will answer differently.

#### Identity

- **A session id names a session, not a place.** Deleting an id and creating it
  again yields a session with storage of its own. A handle to the deleted one
  can neither read what its replacement writes nor write into what it reads.
  *Shared:* `agents.SessionRef` is the address and `agents.NewGeneration`
  mints the discriminator. A function that takes a ref cannot be handed a bare
  id, which is the point — carrying the generation as a field beside the id
  made every hand-built handle, every resolve-by-id and every delete-by-name a
  chance to forget it, silently.
- **A handle is bound when it is BUILT**, never on first use. A handle created,
  held, and first touched after its session was deleted and recreated still
  refers to the one it was built for. *Shared.*
- **A constructor where the id names the STORAGE** (`memory.NewFileSession`,
  `sessions.New`) is a different thing and keeps its meaning: opening it twice
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

- **Monotonic within a session**, so `AfterSeq` orders.
- **Never reused**, including after the entry holding it is removed. A caller
  resuming from the last number it saw would otherwise skip the next append
  forever, silently, its cursor already past it.
- **Never moved for an entry that stays.** Numbering by position in a result set
  shifts every survivor whenever a read filters one out — a compaction pass, an
  item another model produced.
- **`Clear` and `ReplaceEntries` do not restart it.** A cursor outlives the
  entries it pointed at, so a replaced history that renumbers from the beginning
  lands entirely before an existing cursor and is skipped in full.
- One value per entry, whichever API returns it. *All shared.*

#### The tree

- **Parent links are assigned by the minting code**, which is the only layer
  that knows the ids it is about to hand out.
- **A removal never leaves a reference dangling.** Anything pointing at what was
  removed — a child's `ParentID`, a leaf move's target, an update entry — is
  re-pointed or removed with it, atomically. A walk that stops at a missing
  parent reads the session short, so a removal in the middle of a branch
  silently truncates everything before it.
- **A removal operates on the active branch**, not on append order. Removing the
  newest row can otherwise take an entry off a branch nobody is on. *All
  shared.*

#### Removing

- **`EntryPopper.PopEntry` removes the most recent entry**, whatever kind it is.
- **`ItemPopper.PopItem` removes the most recent conversation item**, skipping
  what is not one — a banner, a leaf move, a folded-away entry. It is "undo my
  last message".
- **They are two capabilities because they answer two questions.** One interface
  answering both meant the same call did different things depending on the
  store.
- **A capability is offered by every backend that can support it, or by none.**
  An interface with one implementation, in an internal package, is an API a
  caller cannot use and a doc snippet that fails at runtime. *Shared: a backend
  that can remove an entry gets both, because the selection is shared.* A
  **flat server-held backend** (`openai.ConversationsSession`) still offers
  both: every entry it holds IS a conversation item, so the two pops answer
  the same question and the trivial selection satisfies the shared one.
- **A checkpoint folds entries out of a pop's reach; popping it brings them
  back.** An entry a checkpoint folded is skipped exactly as a banner is — not
  part of the conversation as anyone sees it — while the entries the pass KEPT
  stay poppable: they are on the branch and in the model's view. And since
  `PopEntry` takes the newest entry whatever it is, popping a checkpoint
  UNDOES its fold: the exclusions leave with it. A store that materializes the
  fold (the server's `compacted` flag) reverses that bookkeeping in the same
  step — the checkpoint and the flag are two records of one fact. *Selection
  shared (`PlanPop`); the bookkeeping reversal per backend that keeps any.*

#### The change record

- **Every change moves a session in its listing**, not just an append: clearing
  and popping are changes.
- **It never moves backwards.** A backend that infers the time from stored
  content moves a session back to its creation as soon as there is no content
  left to infer from.
- **A session with no writes yet sorts by when it was created**, not by the zero
  time.
- **A session's metadata and the listing's are the same answer**, read through
  one path. *Shared.*

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
  its session's deletion must REFUSE the write (`ErrSessionNotFound`), inside
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
- **"Did the pass change anything" compares whole entries**, via
  `SessionEntry.Equal`, not the count and not a chosen subset of the fields.
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
itself — never from a copy inside the checkpoint, which is what keeps a later
pop of a kept entry from living on in a duplicate. The folded entries stay in
the session untouched, so a reader can offer to expand them, a fork from
before the checkpoint still finds its full history, and popping the checkpoint
un-folds them (§2.5e2). `ContextEntries` leaves folded entries out and
`ProjectEntries` renders each live checkpoint's summary up front, so the next
run reads the shorter context without recomputing the pass.

Writing a checkpoint is an optional capability (`CompactionCheckpointer`): a
compactor that only reshapes the context in memory is useful and has nothing
durable to say.

**A checkpoint is bound to its pass.** `Checkpoint(compacted)` names the
entries the caller's own `Compact` saw, and a compactor whose state no longer
describes them — one shared across concurrent runs, re-aimed at another
session between the pass and the checkpoint — reports nothing rather than
recording the other conversation's exclusions (and content) here. A lost
checkpoint costs one recomputed pass; a stolen one is a cross-session leak.

The one path that still rewrites is `openai.CompactionSession`, because the
server's compact API returns a replacement rather than a decision.

### 2.5g Context overflow ✅

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
- **A self-compacting storage recovers too.** With no run-level Compactor (or
  with one standing aside because the storage is `CompactionAware`), overflow
  recovery calls the storage's `RunCompaction` with `Force: true` and rebuilds
  the turn's context from the session. Forced, because the storage's own
  trigger normally decides when to compact and an overflow is the one moment
  that question has already been answered — by the provider. The no-op rule is
  unchanged: a forced pass that leaves the history identical buys no retry.
- Detection matches the provider's message, because that is all a context
  overflow arrives as — a 400 with prose in it. Treating every 400 as an
  overflow would compact and retry after a malformed request, hiding a bug
  behind a shrinking conversation. The marker list covers both providers'
  shapes (OpenAI's `context_length_exceeded` family; Anthropic's "prompt is
  too long" and `model_context_window_exceeded`).
- A backend may report overflow in a SUCCESS-shaped response: Anthropic's
  `stop_reason: model_context_window_exceeded` means generation hit the
  window mid-response. The adapter surfaces that as an error carrying the
  marker ([§5.10](#510-non-responses-backends-adapt-at-the-model-boundary)) —
  resending unchanged would stop at the same wall, and compact-and-retry is
  the recovery that actually helps.
- **A truncated response is NOT an overflow** ([§2.7e](#27e-truncated-responses-)).
  Its input fit; compacting the input does not raise the output cap that cut it
  off.
- A recovered overflow is recorded as a `context_overflow` diagnostic.

### 2.5h Crash recovery ✅

`RecoverSession` repairs a session a killed process left inconsistent.

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

### 2.6 Guardrails ✅

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
behavior: concurrent with the model call, with cancellation. ✅

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
- `ToolLoopPolicy.MaxConsecutiveFailures` trips a circuit breaker when that
  many turns in a row have every tool fail, and aborts the run. ✅

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

### 2.7c Tool capabilities are fields ✅

`*Tool` is the only tool type, and everything a tool can do beyond
being called is a **field** on it: `OnInvoke`, `Description`,
`ParamsJSONSchema`, `Strict`, `NeedsApproval` / `NeedsApprovalFunc`,
`Guardrails`, `Timeout`, `Sequential`, `IsEnabled`, `FailureErrorFunction`,
`Deferred`, `RetrySafe`.

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

**Why fields and not an interface with optional side interfaces:** that was the
previous design, and it had exactly one concrete implementation
(`*Tool`) plus eight wrapper shells whose only job was to set what were
already fields on it. The wrappers required a `ToolAs[T]` unwrap walker, and a
bare type assertion through a wrapper silently reported that a tool needing
approval needed none — a trap the design created and then had to specify around.
A field cannot hide behind a wrapper.

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
- **None of its tool calls PAUSE, either.** A truncated call never becomes an
  approval interruption: pausing puts a doomed call in front of a human, and an
  approval serialized into a `RunState` and resumed elsewhere would execute
  what the pausing process refuses. The guard runs before the approval
  partition, and `Status`/`IncompleteReason` survive `RunState` serialization
  so a cross-process resume refuses the same calls.

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
  `RunItem.NestedUsage` hold what a tool spent on model calls of its
  own. It is not merged into `Usage`, because the two answer different
  questions: a nested run's tokens were spent on a different conversation, and
  counting them as context would make this one look larger than anything ever
  sent. It IS part of the run total, since the nested run shares the parent's
  usage.
- `RunResult.UsageByResponse()` and `RunResult.NestedUsage()` read it back:
  where the tokens went, and how many were spent off this conversation.

### 2.7h Schema validation ✅

Tool arguments and structured outputs are validated against the **whole** JSON
Schema, not a root-level `required` check.

- Nested `required`, nested type mismatches, enums and bounds are enforced.
  The old check meant `{"config":{"host":"x"}}` satisfied a schema requiring
  `config.port`, and the tool received a zero value it had no way to notice.
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
- Schemas are compiled **once per tool**, not per call.
- `EnsureStrictJSONSchema` is unaffected: it is the OpenAI strict-mode
  *transformer*, a different job from validation.

### 2.7i Progressive tool disclosure ✅

A tool marked `Deferred: true` is withheld from the model until some
`ToolResult.AddedTools` names it.

- **Marking the tool is the opt-in**, not a run-level flag: the interesting
  question is which tools wait, and a run where everything is deferred could
  never disclose anything.
- **Disclosure is cumulative** for the rest of the run. Withdrawing a tool after
  one use would surprise a model that had just been told it existed.
- **It survives a resume** (`RunState.DisclosedTools`), a serialized
  cross-process one included ([§2.1](#21-the-run-loop-): a `RunState`
  round-trips whole). Re-hiding would look, from the model's side, like a tool
  taken away mid-conversation.
- **It does not override `IsEnabled`.** Disclosure opens a door; it does not
  force one.
- **Naming an unknown tool is ignored.** A tool should not be able to fail a run
  by mentioning something.

### 2.7g Tool progress ✅

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
  stated consumer-side in [streaming.md](streaming.md).
- A **nested agent-as-tool run is streamed whenever the parent is**, so its work
  shows up as progress without the caller wiring `OnStream`. Only its messages
  are forwarded: relaying the nested raw deltas would bury the parent's stream.
- The sandbox exec tool streams stdout through it, capturing in parallel —
  streaming must not cost the model its output.

### 2.7j Sandbox command policy ✅

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

### 2.7k Persistent shells ✅

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
- Reading happens on a background goroutine, because `Terminal` has no read
  deadline and a blocked `Read` on the calling goroutine cannot be interrupted
  by any timer.

### 2.8 Nested agent-as-tool attribution ✅

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

🚧 Only the turn dimension ships today; tokens and deadline are not
implemented. The three dimensions are **OR**-ed: whichever trips first stops
the run.

| Dimension | How it is counted |
|---|---|
| `MaxTurns` | **Model calls.** Not reset by handoffs. A HITL resume continues accumulating. |
| `MaxTokens` | Cumulative `Usage.TotalTokens`. Nested agent-as-tool usage **counts**, because it folds into the parent `Usage`. |
| `Deadline` | A `time.Duration` measured from the start of the run. |

🚧 LLM calls made by compaction itself **count toward `MaxTokens`** but
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

### 2.11b Run control ✅

`Run` returns a `RunControl` alongside the stream. It is safe to use from
another goroutine, including before ranging begins.

RunControl is stop + injection + pending, nothing more. An introspection trio
(`Phase`/`CurrentAgent`/`CurrentTurn`) shipped here for a while and was removed
with zero consumers: every real host renders progress from the stream's own
events, which carry strictly more information. Beyond `StopAfterTurn`, it has
three **injection methods** feeding one arrival-ordered queue; the two
consumption points filter by kind, and only two kinds may extend a run that
was ending:

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
  pending-input list.
- **Nothing is silently dropped.** `Pending()` reports what a run did not
  consume, which is how a caller learns a `NextTurn` arrived too late.
- **Queued input survives an interruption**: `RunState.PendingInput` carries
  it — across serialization too ([§2.1](#21-the-run-loop-)) — so a steer sent
  while a human was deciding on an approval is delivered on resume. That is
  precisely when someone is looking at the run and saying something about it.
  The wire shape stays the three lists, which does not record cross-kind
  arrival order — an accepted loss at the pause boundary, not worth a schema
  bump.

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
  recovered tool panic, a tool timeout a `FailureErrorFunction` converted into
  model-visible output. None of them reach an error return, so without this
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
  conversation into a log aggregator. Outside that opt-in filter, a
  `Sensitive` attribute renders as a redaction marker — its `LogValue` never
  reveals the value, so handing one to your own unfiltered `slog.Logger` is
  safe by default.
- **Every record carries `component`**, so SDK chatter is filterable by origin
  without each call site repeating the attribute.
- **The logger's handler sets the level floor.** Most of what the SDK says is
  `Debug`; hand it a dedicated logger whose handler enables Debug to see it
  without enabling Debug application-wide. (A `Level` override field existed
  and was removed: it ANDed with the handler's own gate, so it could only
  tighten — the loosening its doc promised was impossible.)
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
like `Logging` keeps it by construction); a third-party author implements the
same three clauses directly.

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
logging still logs, `Retry` still retries, `Approval` still resolves further
pauses. (The rule above is what keeps the two from compounding: an in-chain
resume passes stripped options.) The paused state's agent and input are
already decided; a middleware's edits to those fields do not apply on resume.

**Workflow middlewares (`Plan`, `Todo`)** rewrite the ENTRY agent only —
handoff targets keep their own toolset, the same scoping as every
instruction-injecting middleware. Their invariants:

- **Plan gates by hiding, not failing.** While planning, tools outside the
  read-only set are absent from the model's toolset (direct tools via their
  enabled hook; MCP tools filtered out of each turn's listing; handoffs via
  `Handoff.IsEnabled` — a target's full toolset would otherwise be a side
  door out of plan mode), so a write cannot even be attempted. Every gate
  COMPOSES with the predicate it wraps rather than shadowing it — the
  resolver consults only the outermost layer, and unlocking the plan gate
  must not resurrect a tool or handoff the host itself disabled. Read-only-ness
  is a NAME LIST (`DefaultReadOnlyTools` when nil), not a tool capability:
  tools carry no side-effect marker, and a visible, editable list beats an
  interface nobody remembers to implement.
- **The plan review is an ordinary approval pause.** `submit_plan` is
  approval-gated always; the plan text is the call's arguments. Approving it
  unlocks the toolset and the SAME run continues; rejecting feeds the message
  back and planning continues, write tools still hidden. No second pause
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

### 2.13 Background tasks ✅

A task is a sub-agent that outlives the turn that started it. The invariants
below are behavior, not implementation detail — see [tasks.md](tasks.md).

- **Identity and execution are separate.** `Task.ID` is the durable entity,
  `Task.RunID` one attempt at it. Collapsing them makes a retry inexpressible
  without inventing a second task.
- **Finalization is a compare-and-set**: status, result and the wake-up debt in
  one atomic transition, only while the task is non-terminal. This is why a
  task store must be transactional, and why no file-backed one is offered — a
  read-modify-write cannot arbitrate between two finalizers.
- **Four reasons not to wake a parent**, all of which must be clear: it is
  being deleted, it has a live run, it is paused on a human decision, or the
  guard could not tell. **A guard that cannot answer must refuse** — "cannot
  prove it is safe" is not permission. A refused wake KEEPS the debt.
- **A cancellation never wakes**, nor does a result the model already pulled
  with `task_status`. Both would burn a turn restating what is already known.
- **A task row names sessions by GENERATION, not by id.** A session id names a
  session, not a place (§2.5e2), and a task outlives the turn that spawned it
  by design — so a row matched on the id alone attaches itself to whatever
  session holds that name later: the replacement lists a dead incarnation's
  tasks, is woken for results it never asked for, and the debt is retried at
  every restart forever. A store that persists task rows binds the parent's
  and the child's generation when it writes one, and every by-session read
  compares them against the generation answering to that id NOW. **A store
  that owns both tables also deletes task rows with the session**, in both
  roles: the generation makes a surviving row inert, the cascade is what stops
  it surviving. *Per backend, because only a backend that holds both can
  answer it; an in-process store has no incarnations to confuse.*
- **A notification line is machine-readable, and its fields come from
  untrusted text.** A label and a result are model output; formatting escapes
  the line delimiter AND the field delimiter, because the line pattern's own
  greediness otherwise lets a crafted result re-aim the task id and status on
  the very same line — forging a card for a task the sender does not own.
- **A wake-up runs under the configuration snapshotted at spawn**, not resolved
  fresh: the parent may be configured differently by then.
- **A restart fails what it interrupted** and owes each parent a wake-up, so
  the news is delivered rather than lost. `input_required` is left alone: its
  approval persists.
- **`input_required` is not terminal.** A task waiting on a human is in flight;
  a notification for it would announce something that has not happened.
- **A paused task is claimed before the host is told to stop it** (the finalize
  is the exclusive claim against a concurrent approval); a working one has its
  run cancelled first, or the run's own completion records a success for
  something the user stopped.
- **Rollback of a half-finished spawn uses a detached context.** `Spawn` runs
  inside the parent run, so a parent cancellation racing it would kill the
  cleanup halfway.
- **A depth check that cannot be made refuses**, the same rule as the wake
  guard. Depth is read from the store, and a lookup that fails is not the same
  answer as "this parent is not a task" — that one restarts the count at 1, so
  treating them alike makes one transient query error a way past the limit.
  `MetaFor` reports the failure rather than resolving it to "no".
- Defaults: depth 1 (a task cannot spawn tasks), 6 concurrent tasks per parent,
  300-rune summaries, a 120s bound on `task_status`'s wait.
- **A notification is a user-role entry** the model reads verbatim; a UI renders
  it as a card. Formatting and parsing ship together, so the format is defined
  once.

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
| Strict schemas | on | Chaining `NonStrict()` relaxes both the advertised schema and local validation, atomically |
| Handoff input schemas | strict | `Handoff.NonStrictSchema: true` opts out; the zero value is the strict default |
| Tool errors | fed back to the model | `DefaultToolErrorFunction`; set the field to `nil` to make them fatal |
| Tool concurrency | unlimited | Bound with `MaxToolConcurrency` |
| Input guardrails | concurrent with the model call | `Blocking: true` makes one a gate |
| Session persistence | after each turn | Final turn is written after output guardrails pass |
| `RunResult.Usage` / `RunState.Usage` | detached snapshot | Never the live accumulator; read without synchronization. Mid-run, `RunContext.Usage` is live — read it via `Snapshot()` |

---

## 5. Recorded design decisions

These have been discussed and settled. Read the rationale before reopening.

**A decision is only as good as the reason recorded under it.** Some entries
here were settled early, when the SDK was still a port, and their stated reason
is a fact about the Python SDK rather than about this one. Those are marked
**🔁 reason under review** — the decision still stands and the code still
matches it, but the *justification* is not load-bearing, so a proposal to change
one should be weighed on its own merits instead of being closed by citation.
Anything unmarked has a reason that stands on its own.

When re-deciding a marked entry, replace the citation with a reason that would
convince someone who had never seen the Python SDK — or change the decision.
Either way, drop the mark in the same change.

### 5.1 Handoffs stay; graph orchestration does not replace them

A handoff is "switch agent at runtime"; a graph is "declare the topology up
front". They solve different problems. Our handoffs carry an `InputFilter` and
history folding; the equivalent in a graph model takes a lot of glue. Graph
orchestration, if it ever arrives, belongs *above* handoffs — serving task
orchestration, not replacing agent switching.

### 5.2 Type names stay 🔁 reason under review

`RunItem`, `RunResult`, `RunContext` and friends came from the Python SDK, but
they read fine as Go. Renaming buys nothing except "looks less like Python" and
breaks every caller.

**Under review**, because "looks less like Python" is not the only thing at
stake and this entry has been used to close cases where it was not the point:

- `Get`-prefixed methods (`Model.GetResponse`, `ModelProvider.GetModel`,
  `Instructions.GetInstructions`, `PromptProvider.GetPrompt`,
  `Agent.GetSystemPrompt`, `Agent.GetPrompt`) are direct transliterations of
  `get_*`. Go accessors do not carry the prefix, and this is a rule about Go,
  not about Python.
- The `T`-prefixed aliases (`TResponseInputItem` and friends) spell a Python
  `TypeAlias` convention that has no Go counterpart. [§5.5b](#55b-the-wire-types-couple-our-compatibility-to-openai-gos)
  already schedules them for the next breaking window.
- `AgentsError` stutters (`agents.AgentsError`) and carries a `nolint`
  admitting it.

The rule that survives is the second half: **a rename is a breaking change and
is batched**, not taken piecemeal. The window in §5.5b is where these belong.

### 5.3 `Instructions` and `Prompt` both stay 🔁 reason under review

`Prompt` (a server-stored prompt template with a version and variables) is a
**Responses API capability**, not a porting artifact. The two compose: a stored
prompt provides the base, instructions append to it.

**Under review**, because this settles only *whether both concepts exist* — the
answer is still yes — and has been read as settling their *shape*, which it
never addressed. Both `Instructions` and `PromptProvider` are single-method
interfaces (`GetInstructions`, `GetPrompt`) whose only implementations are
unexported types in this package; nothing outside the SDK implements either.
A func type would carry the same capability with no adapter types, and would
drop `Agent.GetSystemPrompt` / `Agent.GetPrompt` — both five-line forwarders.
That is a separate decision from this one, and it is open.

### 5.4 A tool is a struct, not an interface

`*Tool` is the tool type. There is no `Tool` interface, which is how the
"no hosted tools" decision ([§1.2](#12-non-goals)) is enforced: a provider-hosted
tool has nowhere to be introduced, because there is nothing to implement.

This replaced a sealed interface with an unexported marker method. The seal was
doing the same job, but it also invited a wrapper hierarchy to carry optional
behavior, and that hierarchy needed a lookup protocol
([§2.7c](#27c-tool-capabilities-are-fields-)) to be usable. A struct closes the
kind and carries the behavior in one move; behavior stays open because the
fields are exported and a variant is a copy.

### 5.5 Internal item types are Responses wire types

Zero conversion, zero information loss — reasoning ids, `encrypted_content` and
strict schemas all survive round-trips. The cost is that non-LLM entries need a
`SessionEntry` wrapper to have somewhere to live.

### 5.5b The wire types couple our compatibility to openai-go's

§5.5's zero-conversion choice has a price with a name: `TResponseInputItem` and
friends are **type aliases of `openai-go/v3` union types**, and they appear in
nearly every exported signature. A major-version bump of openai-go (v3→v4) is
therefore a breaking change of this SDK's **entire API surface**, whatever else
it contains.

This is accepted, not overlooked:

- Wrapping the wire types behind our own structs would cost the round-trip
  fidelity §5.5 exists for, plus a conversion layer that must chase every
  Responses API addition forever.
- The major version is pinned in `go.mod`; nothing forces a bump on users
  until we take one deliberately.
- **When a bump does come, it is the merge window** for every other
  API-surface change on the shelf — e.g. renaming the Python-inherited
  `T`-prefixed aliases (§5.2 does not protect names whose surface is breaking
  anyway) — so users absorb one deprecation cycle (§5.8), not two.

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

### 5.9 A parent-linked checkpoint chain for execution state is declined

Microsoft's agent-framework-go checkpoints every workflow superstep into a
parent-linked store (`CreateCheckpoint(..., parent)` /
`RetrieveIndex(withParent)`), so a run can resume from **any** historical
point and the checkpoints form a browsable tree — time-travel debugging
included. It needs that structure because its `Session` is a key-value bag:
the checkpoint tree is its only history.

Declined here, because this SDK already carries the stronger halves of that
design:

- **The session tree is the parent chain** (§2.5d). "Re-run from message X"
  and "same history, different model or options from turn N" are session
  branches: a new run starts from any leaf, its model input rebuilt by
  projection.
- **`RunState` serializes the one state that cannot be rebuilt** — the
  mid-turn pause with tool calls awaiting approval (§2.7) — and resumes
  across processes. Between turns, the session is the truth; the rest of the
  runner's state is derivable or expendable.
- **Per-turn persistence bounds crash loss to the in-flight turn**, and
  repair (§2.5h) makes the session loadable again.

The net capability a chain would add — deterministic replay, and a byte-exact
"resume turn N with the execution state it had then" — does not justify a
second history structure beside the tree, with its own consistency rules
against it.

Revisit only with a concrete replay/debugger need, and then on three terms: a
checkpoint is a **session entry kind** (payload: a trimmed `RunState`,
projected to nothing), so the tree stays the only history structure; a
deterministic execution mode comes first, because replaying a
nondeterministic run replays into different behavior; and the payload must be
trimmed — `RunState` carries every raw response, and a per-turn copy of that
grows quadratically.

### 5.10 Non-Responses backends adapt at the model boundary

The canonical item and event format stays the Responses wire format (§5.5)
even when the backend speaks something else. An adapter translates in both
directions **inside its own package** — `models/anthropic` for the Messages
API — so the runner, sessions, run state and the server never learn a second
format. `models/modelkit` (root module) holds the shared halves: the input
walker, item/event synthesizers that stamp round-trippable raw JSON, and the
feature-rejection helper.

The runner's consumption contract, which every `agents.Model` implementation
in this repository must satisfy (enforced by `modelkit/conformancetest`; both
in-repo providers run it):

- **Output items** are canonical items whose `RawJSON()` is non-empty wire
  JSON — `agents.OutputToInput` and session persistence depend on it. The
  types the runner models are `message` / `reasoning` / `function_call`;
  anything else rides through as an `ItemUnknown` run item.
- **Stream vocabulary** is `response.*` only. The first event is
  `response.created`; each finished item gets one `response.output_item.done`
  (in order); the terminal event is `response.completed` or
  `response.incomplete` — reason `max_output_tokens` is the one recoverable
  truncation (§2.7e). Text streams as `response.output_text.delta`, raw
  reasoning text as `response.reasoning_text.delta`. These names are
  load-bearing: the agents-server UI renders exactly these events.
- **Usage** is Responses semantics: `InputTokens` is the TOTAL input count,
  cache reads and writes included; `CachedTokens` / `CacheWriteTokens` are
  informational subsets. A backend that reports uncached input separately
  (Anthropic) adds the parts.
- **Unsupported request features fail loudly** — a `*agents.UserError` naming
  the feature (`modelkit.Reject`), never a silently dropped setting.
- **Continuity blobs** (thinking signatures, redacted reasoning) ride in the
  reasoning item's `encrypted_content` — the one canonical slot that survives
  `OutputToInput` and session storage. A reasoning item without one is
  dropped on replay to a backend that requires signatures.

Anthropic-specific mappings recorded with the adapter: mid-history
system/developer messages travel as `mid_conv_system` blocks in system turns
(the Messages API has no plain `system` role for input text; top-of-run
instructions use the top-level `system` parameter); `thinking` ↔ `reasoning`,
with the blob in `encrypted_content` carrying an adapter prefix
(`thinking_signature:` / `redacted_thinking:`) — a blob without a recognized
prefix is another provider's reasoning and is dropped on replay rather than
sent as a bogus signature; `stop_reason: max_tokens` →
`incomplete`/`max_output_tokens`; `stop_reason: refusal` → ONE canonical
refusal message and nothing else (the response's text, else
`stop_details.explanation`, else a fixed line — never empty): the Messages
API reports refusal out-of-band, and a refused response's partially
generated `tool_use` blocks must not survive into items the runner would
execute before it ever looks for the refusal — so `ModelRefusalError` and
`model_refusal` handlers fire exactly as on any backend (a streamed
refusal's mid-stream `item.done` events may still show text/tool items;
the terminal rebuild is what the runner reads);
`model_context_window_exceeded` → an error carrying that marker (§2.5g);
`Reasoning.Effort` → thinking budgets (minimal 1024 / low 4096 / medium
16384 / high 32768) with `MaxTokens` defaulting to 8192 (grown to
budget + 8192 when the budget would not fit under it), and thinking rejects
`Temperature`/`TopP`/forced tool choice up front; prompt caching is the
request-level `cache_control` marker, on by default
(`Provider.WithPromptCaching(false)` opts out). `models/anthropic` is a
submodule per §5.7 — it carries the anthropic-sdk-go dependency; `modelkit`
adds none, so it stays in root.

### 5.11 Construction errors split by data provenance

A constructor whose failure can only be a programmer error **panics**; a
constructor whose input is runtime data **returns an error**.

`NewTool` and `AgentAsTool` derive their schema from a Go type: for a
given type the outcome is deterministic, so a failure (non-struct args, a field
no schema can express) is a bug that any test constructing the agent surfaces
immediately — the `regexp.MustCompile` precedent. They panic, which also keeps
constructors chainable inside `Agent{Tools: []Tool{...}}` literals.
`NewRawTool` takes a schema that is data (loaded from a database or
config), so a bad schema is an expected input, not a bug: it returns
`(*Tool, error)`.

The earlier design — returning a tool that errors on every invocation, surfaced
by the runner before the first model call — deferred a deterministic bug to
runtime and cost a field (`constructionErr`) plus a runner check. Rejected.

### 5.12 One user-context entry point

`RunOptions.Context` is the only way user data enters a run; every run wraps it
in a fresh `RunContext`. There is no field to inject a pre-built `RunContext`:
nothing in the SDK needed it (nested runs share the parent's `Context` value
and fresh accumulators), and cross-run usage totals are sums over each
`RunResult.Usage`. Two fields expressing one concept was the cost; a run owning
its `RunContext` outright is what the guarantee "a run's accumulators start
empty" rests on.

### 5.13 AgentToolConfig configures the tool, ModifyRunOptions the run

`AgentToolConfig` holds only what has no `RunOptions` counterpart: the tool's
name, description, visibility, approval gate, error rendering, output
extraction, streaming callback and input rendering. Everything about the
nested run itself — session, turn budget, conversation, model, guardrails —
goes through the single `ModifyRunOptions` channel. Mirror fields
(`MaxTurns`, `Session`, `ConversationID`) were removed: each was a second
spelling of a `RunOptions` field, and the escape hatch's existence proved the
dedicated-field approach could never be complete. A `ConversationID` set via
`ModifyRunOptions` is still cleared when a paused nested run resumes (the
serialized state already carries the conversation).

### 5.14 Sandbox file tools share exec's path view

The sandbox file operations (`ReadFile`, `WriteFile`, `CreateExclusive`,
`ListDir`, `RemoveFile`, `Rename`) resolve paths with **shell semantics,
identical to `exec_command`**: a relative path resolves under the working
directory, an absolute path is used as-is. The isolation boundary is the
sandbox itself, not the working directory — for local, ssh and
docker-persistent, exec already reaches everything on that filesystem, so
pinning the file tools inside `WorkDir` adds no protection; it only creates a
second path universe. The model learns real absolute paths from exec output
(`pwd`, `ls`, `git status`) and echoes them into the file tools, so the two
surfaces sharing one view is what makes those calls work. (An earlier
workdir-rooted "virtual chroot" design was dropped for exactly that failure:
absolute paths got re-joined under `WorkDir` and read as "not found".)

**The one exception is docker bind-mount mode**, where file operations run on
the *host* side of the mount while exec runs inside the container — the
container's isolation does not cover them. There they are confined to
`WorkDir` via `os.Root` (which also polices `..` and symlink escapes);
absolute paths must lie under the in-container mount point (`/workspace`, the
only view the model ever sees) and are translated to their host-side names,
and anything else fails with `sandbox.ErrOutsideWorkDir` — an explicit
"outside the working directory" to the model, never a silent re-rooting.

### 5.15 Streaming-only backends adapt with a Model decorator

Some backends accept **only** streaming requests — the ChatGPT Codex backend
(`chatgpt.com/backend-api/codex`) rejects a non-streaming POST with 400. The
adaptation is `NewStreamOnlyModel` / `NewStreamOnlyProvider`: a
provider-agnostic decorator whose `GetResponse` runs the request as an
internal `StreamResponse` and assembles the final `ModelResponse` from the
terminal event; `StreamResponse` passes through untouched.

It is a **Model decorator, not an HTTP middleware**, because forcing
`"stream": true` at the transport layer would hand an SSE body to a caller
that parses a JSON response — the request shape and the response parser must
switch together, which only the model boundary sees. Assembly is shared with
the runner's own streaming path (one `responseAssembler`), so the two paths
cannot drift; like that path, the assembled response carries no `RequestID`
and treats a length-truncated `response.incomplete` as an arrived (not
failed) response. Compose it **innermost**, directly on the backend it
adapts: decorators above it (retry, fallback, routing) then see a severed
stream as an ordinary `GetResponse` error and handle it normally.

### 5.16 A severed stream retries only before output, with the preamble held back

Three rules govern a stream that dies before its terminal event:

- **Classification**: `modelkit.RetryableError` treats `io.ErrUnexpectedEOF`
  as a transport failure (retryable), alongside `net.Error` — a gateway or
  proxy severed the connection mid-event.
- **Truncation is an adapter obligation.** A connection severed *at* an event
  boundary looks like a clean EOF to the SSE layer — no transport error, the
  stream just ends. An adapter must not let that pass as a normal finish:
  when the stream ends cleanly without a terminal event
  (`response.completed` / length-`incomplete`; `message_stop` on Anthropic),
  it surfaces `modelkit.TruncatedStreamError`, which wraps
  `io.ErrUnexpectedEOF` so the classification above applies. Without this the
  failure would fall through to the runner's "ended without a completed
  response" — accurate, but unretryable. (The runner keeps that check as the
  last line of defense for models that don't.) Symmetrically, a transport
  error AFTER the terminal event is not surfaced: the response is complete
  and delivered, and failing the call then would throw away a valid result
  over a connection with nothing left to say.
- **Commit window, with pre-commit events held back**: `NewRetryModel` and
  `NewFallbackModel` may replace a broken attempt only while nothing has been
  **generated**. Two event classes carry nothing the model generated:
  lifecycle preamble (`response.created`, `response.in_progress`,
  `response.queued`), which arrives the moment the connection opens, and
  terminal-failure events (`error`, `response.error`, `response.failed`) —
  replacing an attempt that ends in one of those is the whole point, and the
  streaming chain must advance on a `response.failed` exactly like the
  blocking chain advances on the error it becomes. (`response.incomplete` is
  NOT in this class: a length-truncated response is output that arrived, and
  committing on it is what stops a retry from throwing it away.) Once
  delivered, though, such an event would commit the consumer to a response a
  second attempt then duplicates — so the decorators buffer them
  (`deliverStreamAttempt`): an abandoned attempt's pending events are dropped
  and the consumer sees exactly one coherent response, with a `model_retry`
  span and a `DiagModelRetry` diagnostic as the only trace of the failed
  attempt. Pending events are flushed when the attempt turns out to be the
  stream's last word — the first output event commits it (from there, errors
  pass through, recorded as `DiagStreamError`), and a clean all-pending
  finish or a terminal failure delivers them before the verdict. A nil event
  neither commits nor buffers (dropped, as the runner does), and a consumer
  that stops mid-flush ends everything — no further events, no diagnostics.

---

## 6. Open questions

When a new case comes up that this document does not answer, add it here with
the options under consideration. Implementing it means moving it out of this
section and into §2 in the same change.

### 6.2 Two parallel error classifications

Errors carry both an exception-style hierarchy (`AgentsError` embedded into six
concrete types) and an `ErrorCode` string. They can disagree: `CodeOf` ends in
a fallback `switch` on the concrete type precisely because a caller building an
exported error as a struct literal leaves `Code` at its zero value. Embedding
(rather than wrapping) also means `errors.As` cannot reach the base, which is
why `agentsErrorCarrier` + `base()` + `AsAgentsError` exist.

Options: keep `ErrorCode` as the single classification (it survives transports,
which is its real value) with sentinel errors for `errors.Is`, and keep a
concrete type only where it carries extra fields (`MaxTurnsError.MaxTurns`,
`GuardrailTripwireError.Result`) without embedding the base; or keep both and
specify which wins.

### 6.3 `RunErrorDetails` duplicates `RunResult`

Seven of `RunErrorDetails`'s fields (`Input`, `NewItems`, `RawResponses`,
`LastAgent`, `Usage`, `GuardrailResults`, `Diagnostics`) are `RunResult`'s,
verbatim. A failed run and a completed one describe the same thing.

Options: carry a `*RunResult` on the error; keep both and document the split.

### 6.4 `agents` is one package

73 non-test files, ~15k lines, ~450 exported symbols — against 67 for the next
largest package in the repo. The session/entry layer (`session_*.go`) and the
run loop (`run_*.go`) are only weakly coupled and could separate.

This is deliberately last: [§6.2](#62-two-parallel-error-classifications) and
[§6.3](#63-runerrordetails-duplicates-runresult) both delete code, and
splitting first would move the same code twice.

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
