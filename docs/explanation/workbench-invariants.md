# Workbench design invariants

Rules every `agents-server` panel, handler and store change must respect. Each
exists because its violation shipped a real bug; the fix for a new feature is
to fit these shapes, not to add a one-off patch beside them. When a change
genuinely does not fit, this list is updated in the same PR.

These are the workbench's. The SDK underneath has its own, in
[the spec](../reference/spec.md), and the reasoning behind them in
[design decisions](decisions.md).

---

Cross-cutting rules — and, from 29 on, recorded per-feature decisions — every
panel/handler/store change must respect. Each exists because its violation
shipped a real bug; the fix for a new feature is to fit these shapes, not to
add a one-off patch beside them.
When a change genuinely doesn't fit, update this list in the same PR.

**API shape**

1. **`config` blobs travel as JSON objects, never strings.** Every
   backend-specific settings blob (`mcp_servers.config`, `sandbox_configs.config`,
   `guardrails.config`) is a `json.RawMessage` exchanged as an inline JSON
   object. The frontend reads and writes it as an object — no
   `JSON.stringify`/`JSON.parse` of the field itself. (The guardrail panel once
   sent a stringified config; every save failed with 400 and nobody noticed.)
2. **List responses carry every field the edit form needs.** `useCrud` panels
   initialize their edit form from the list item. A list-side projection that
   drops fields (config, flags) makes the edit form silently wipe them on the
   next save. Either return full rows from List or make the panel fetch Get
   before editing — never assume "the list is just for display".
3. **Derived state is computed in one backend function; the frontend renders it
   verbatim.** Connection/login/authorization lifecycle is reported as a single
   `status` (MCP: `disabled | connecting | authorizing | needs_auth |
   disconnected | connected`) or boolean (`chatgpt_logged_in`,
   `has_oauth_token`) derived server-side from the facts it already owns
   (manager/coordinator state + stored config). The frontend must not
   reconstruct state by combining multiple response fields or its own
   per-item maps — that is how phantom states (`auth_state === 'authorizing'`)
   and stuck buttons happen.
4. **Keep swagger annotations in sync with the actual response type**, and run
   `make openapi` after any handler change — CI diffs the generated spec.

**State & lifecycle**

5. **An off switch must hold at every entrance.** When a resource has
   `enabled=false`, every path that could activate it (manual connect
   endpoints, agent assembly, startup auto-connect) must respect the flag —
   agents pick MCP tools by live connection, so one unguarded connect path
   voids the whole switch.
6. **Create and Update trigger the same side effects.** If updating a resource
   reconciles a live connection, creating one must too. Asymmetry here reads
   as "sometimes it just doesn't work".
7. **Async settling uses grace-window polling, not per-item timers.** After a
   mutation whose effect completes in the background (reconnect, OAuth), the
   panel polls the list while any row is in a transitional status or an
   ~8s post-mutation grace window is open, then stops. One-shot notifications
   (popup `postMessage`) only trigger an immediate reload — they must not own
   state cleanup, because they don't always arrive. Long-lived per-item
   `setInterval` + hand-rolled cleanup is the pattern that produced the
   5-minute stuck "Authorizing..." button.
8. **In-progress buttons stay retryable when the wait is on an external actor.**
   If completion depends on the user finishing a popup flow, the button that
   started it must allow a superseding retry (cancel the stale attempt, start
   fresh) instead of disabling itself until a timeout.

**Secrets**

9. **Secrets are write-only and go through `handler/secrets.go`.** Read side
   masks with `********`; write side resolves the sentinel (mask = keep stored,
   `""` = clear, anything else = replace). New secret fields must use the same
   sanitize/restore helpers and get a round-trip test — no ad-hoc masking.
   **A mask never round-trips across a destination change**: restoring a
   stored key under a changed `provider_type` OR `base_url` sends one
   backend's real credential to another provider or endpoint, so the update
   is rejected (agents) and fallback entries restore strictly by
   `(normalized provider_type, normalized base_url, model)` — never by
   position; an unmatched mask clears.
   **The mask resolves inside the store's transaction.** The store's `Update`
   (providers, agents, MCP servers) and `SettingStore.Modify` read the stored
   row — `FOR UPDATE` on PostgreSQL — and hand it to the handler's callback
   before writing; a handler must never `Get` first and resolve outside, or
   two concurrent edits let a client echoing the mask restore the key the
   other just replaced. A rule the callback refuses returns
   `badRequestError`, which `saveError` maps. The second accepted form is
   the revision CAS (sandboxes, projects): the handler reads the row,
   resolves the mask outside the transaction, and writes under the row's
   `expected_revision` — a concurrent edit moved the revision, so the write
   is refused (409) rather than landing an echoed mask over a key it never
   saw.
10. **OAuth-class tokens never leave the server.** Store them in their own
    column with `json:"-"`, exclude the column from CRUD updates
    (`ExcludeColumn`), and expose only a derived boolean
    (`has_oauth_token`, `chatgpt_logged_in`). Do not reuse a masked token
    string as a truthiness signal.
11. **An OAuth grant persists as a self-contained refreshable unit, through one
    writer.** The stored payload carries the token AND its refresh context —
    token endpoint plus (possibly dynamically registered) client credentials —
    and every mutation flows through `mcpservers.persistGrant`: the initial
    authorize and every later refresh, including a rotated refresh token
    (`persistingTokenSource`). Restored and live connections must use the same
    refreshing `oauth2.TokenSource` machinery — never a static snapshot of the
    access token, which is exactly the two-mechanism drift (in-process refresh
    worked, restart silently didn't) this replaced.

**Store layer**

12. **No bun `default:` tags on booleans.** bun swaps a zero-value field for
    SQL `DEFAULT` on insert, so `default:true` silently enables a row created
    with `enabled=false`. Use `notnull` and set the value in Go.
13. **Deleting a referenced resource fails loud at use, never silently skips a
    safety feature.** Guardrail names that no longer resolve fail the agent
    build (a guardrail that appears enabled but never runs is a security hole);
    dangling MCP/skill ids are filtered with a visible count in the UI. Pick
    one of those two behaviors deliberately for any new reference.

**Chat / run streaming**

14. **Run events are a broadcast bus PER OWNER, not a reply channel.** Every
    authenticated WS connection of the session's owner is attached to the
    run's stream — on connect (all of that user's live runs, with replay) and
    through `Runner.OnRunAttach` when a run starts or resumes, whether it was
    created over WS or REST — and nobody else's connection hears a thing.
    Two browsers on the same session both watch the conversation live;
    `run.started` carries the prompt (`input`) so a browser that didn't send
    it can render the user bubble. Never wire an event to "the connection
    that asked" — that is exactly the bug this replaced. The bus is only as
    good as a late joiner's ability to PLACE what it hears: `run.started` is
    the sole event that maps a run id to its session, and the client drops
    every event of a run it never saw start, so the hub pins each run's latest
    `run.started` outside the 512-event replay ring and hands it to a
    subscriber whose cursor lies before it, first, whenever the ring itself
    no longer will (`RunHub.SubscribeSeq`) — a reload a minute into a run
    used to show the session idle until it ended. The client, in turn, does
    not re-subscribe on a `run.gap` whose range the ring has already evicted
    (`last_good: 0`, or the cursor it already asked for): that would replay
    the whole ring every few seconds for the run's life.
15. **Protocol constants have one definition per side.** Event types
    (`run.error`, …) and error codes (`session_busy`, …) live in
    `internal/protocol` (Go) and `src/lib/protocol.ts` (TS mirror). Emitters
    and consumers reference the constants, never string literals — a typo must
    be a compile error, not an event that silently never fires. Adding an
    event means updating both files. The mirroring obligation stops at the WS
    contract: the REST error envelope (`internal/protocol/apierror.go`) is
    Go-only, because it has two Go emitters that cannot import each other
    (`handler` and `server`) but only one browser consumer, and that consumer
    reads `error.message` alone — it never branches on the code.
16. **A streamed turn must equal its reload.** The streaming path
    (`src/lib/streamReducer.ts` pure transforms, applied by `useAgentSocket`)
    and the replay path (`buildTimeline` over persisted ENTRIES) must produce
    the same `turn.parts`; `src/lib/timeline.test.ts` pins this isomorphism — run
    it via `npm test`. Intentional differences are documented and asserted
    there (currently: handoff parts are live-only; a rejected call's status
    replays as completed). A new part type or field lands on BOTH paths plus
    the shared types in `timeline.ts`, or the test fails.
17. **Terminal run events reconcile against the store.** Every terminal event
    handler (output/error/cancelled) applies its optimistic parts and then
    reloads the persisted timeline as the authority. Exceptions must be
    deliberate and listed here — currently only `guardrail_tripwire`, which
    skips the reload to keep the retracted-answer view the SDK never persists.
18. **The streaming block patches the DOM; user intent beats the pin.** The
    live text is morphdom-patched (`StreamingMarkdown.tsx`), never rewritten
    via innerHTML — node identity is what keeps a text selection alive across
    deltas, so anything that replaces those nodes wholesale is a regression.
    Bottom-following (`useScrollToBottom`) re-fires on every content growth
    (the dep includes streamed text length) and yields to explicit user
    intent: an upward wheel/drag or an actively changing selection unsticks
    immediately; a stale leftover selection must NOT block re-sticking when
    the user scrolls back down (recency windows, not standing state, arbitrate
    the races with the pin's own trailing scroll events).
19. **A branch move obsoletes every client view of the old path.** Regenerate
    and attempt-switch are server-side appends (`POST /sessions/:id/branch`);
    the client reconciles by refetch, and four rules keep the abandoned
    attempt from lingering on screen: the timeline's `on_path === false`
    filter applies even when no fork exists yet (right after the switch the
    old answer is the user entry's ONLY child — the new attempt has persisted
    nothing); a branch move bumps the session's timeline generation so a
    fetch launched before it is dropped on resolve, never applied; the
    live-tail merge re-appends only the CURRENT live run's turn
    (`mergeLiveTail(…, liveRunId)`); and a pending approval whose run has
    off-path entries stays out of the timeline — the row itself is kept, so
    switching back to the paused attempt re-admits its card and the pause can
    still resume.

**Background tasks**

20. **A task is a durable entity; a run is one execution of it.** `spawn_task`
    mints separate ids: the task row carries `run_id` (its current attempt),
    and `run.started` / `RunInfo.task` carry `task_id` — clients route events
    by run id and key task state by task id — `task_retry` mints a new run id
    on the same task row, and `run.started` carries the `attempt` so a client
    whose card shows a finished task can tell a new attempt from a replay. The transcript lives in a hidden child session. The
    spawn target is an agent config by name; an empty `agent_name` (or the
    `default` / `self` / `current` aliases) runs the task with the spawning
    run's own agent — a config actually named that way wins. Task events use
    the same broadcast bus, replay cursors, approval persistence, and
    retention as chat runs — a task-specific transport is how the two
    lifecycles would drift.
21. **The spawn card's durable truth is an appended update entry.** The hub's
    RunInfo is GC'd minutes after a run ends; when a task changes state the
    server APPENDS an update entry addressed to the spawn call's id — the
    label and summary on display's first-class Title/Summary,
    `task_id`/`task_status` in Extra as renderer state — and the read folds it
    into that call's display. The fold merges non-empty fields only, so a
    retry's working update cannot blank the failed attempt's summary — a
    summary-carrying update therefore also records `task_summary_attempt`
    (Extra merges per key), and the timeline drops a folded summary older than
    the card's `task_attempt` rather than show a voided failure as the current
    attempt's result. Appending is what removed the retry loop: a fast
    task can finish before the turn that spawned it is persisted, and the old
    rewrite hunted for a row that did not exist yet. An update may be stored
    BEFORE its target; folding associates them by call id afterwards. A
    non-terminal update is dropped when the task row is already terminal, so a
    reordered notify cannot roll a finished card back. The live path mirrors
    the fold: pre-terminal status rides run events (the card's badge), and a
    terminal outcome lands on the spawn card live via `syncTaskCard`
    (`lib/streamReducer.ts`, called from the task router in
    `lib/taskEvents.ts`) — same shape as the replayed fold, terminal only,
    never backwards, and re-applied by the chat's `run.tool_call` handler
    when the task's independent subscription outran the card. Durable status
    always comes from the folded entry, never from the hub after the fact.
    Completion wakes the parent at its next run boundary via a
    `[task-notification] ` input;
    the debt is a `wakeups` row inserted in the same transaction as the
    terminal status (invariant 32), consumed by an in-turn `task_status`
    read or delivered by the wake-up run — the auto-wake survives restarts
    via the startup sweep. The notification is ordinary user-role input identified by its text
    prefix. It never renders in the timeline — the chat top bar's Tasks button
    and the Inspector are the human-facing surfaces; the model reads the text
    verbatim. The prefix carries no privileged behavior: a user typing it
    merely hides their own message from the transcript view.
22. **The right side panel is a single-instance Inspector.** Traces, context
    usage, the task list, and one task's detail (live transcript + trace,
    assembled with the same streamReducer/timeline code as the chat) are lenses
    of one panel — a new inspection surface is a new lens, not a second drawer.
    Task detail accumulates live child-run events only while open
    (watchTask/unwatchTask). The persisted span SUMMARY loads on session open —
    the chat labels each turn with its run span's duration, so the data can't
    wait for a lens — but a span's PAYLOAD stays lazy: a stored generation span
    can carry a whole model request (nearly all of a session's trace bytes),
    fetched whole only when its row is expanded (loadSpanPayload). Live runs
    stream their spans over the WS regardless. Run LINEAGE — which
    run's spawn a wake-up belongs to, what nests the "task result" card under
    its originating card — is recorded on the trace itself
    (`trace_events.parent_run_id`, stamped at launch from
    `LaunchRequest.ParentRunID`) and read directly. It is never derived from
    task rows, notification text or the rendered timeline: each derivation
    broke on a surface that does not carry its inputs (a fork copies traces
    but not task rows; a fold moves the notification out of the timeline).
23. **A task's terminal state is written exactly once, via row CAS.** The
    durable row is the terminal authority: `Finalize` (status + full result +
    notification debt in one UPDATE) only wins while the row is non-terminal,
    stop/approve claims race through the same CAS (`Finalize` vs
    `ReclaimWorking` — exactly one wins, and `hub.resume` refuses finished
    records as the second line), and `task_status` treats only the row as
    final — a hub-terminal run whose row hasn't landed is still `working`.
    That second line is one-directional: it rejects a resume that arrives after
    a stop landed, but `taskStopper.Stop` reads the hub status and publishes
    `run.cancelled` after, so the reverse interleaving — a stop that saw
    `RunInterrupted` just before an approval resumed the run — can still put a
    terminal event beside a live segment. The compensation is the post-resume
    task re-check in `ResolveApproval`, which cancels the run it just started
    when the row reads terminal. A new publisher of terminal run events must not
    rely on the guard: give the hub an atomic status transition instead of
    adding a third compensation. A graceful stop marks the hub record before
    signalling, so its clean finish lands as `cancelled` ("stopped after the
    current turn"), never as a completion. A hard cancel is equally
    unambiguous: once the run's context is cancelled, every stage that notices
    it — including a session lookup that never got to answer — reports
    `run.cancelled`, never a `config_error` or `session_not_found` the user did
    not cause, and the task's status follows that event. Deleting a session
    stops its run tree first (cancel + bounded wait on the done gate) so no
    write can land after the
    cascade — which then removes the whole tree, every hidden session at any
    depth, walking LIVE edges only (a stale row's child id may since belong to
    an unrelated session).
24. **One entry in, the same entry out.** The `entries` table stores whole
    `agents.SessionEntry` JSON, with only the columns the queries need lifted
    out. The server does not re-derive a display, a role, or provenance at read
    time — the runner already decided all three, and a reader that recomputes
    them can only produce a worse version that drifts. The messages table this
    replaced had a column per field the UI wanted, so `Source`, `Usage`,
    `Diagnostics`, `NestedUsage` and the parent link had nowhere to go and were
    dropped on the way in. Compaction soft-deletes (`compacted = true`) so the
    UI can still show what was folded, and appends a compaction CHECKPOINT
    whose payload names what it folded (`compaction.excluded_ids`) and carries
    the summary that stands in for it — which is why the model sees
    `[summary, kept…]`: the projection drops what the checkpoint excluded and
    renders its summary up front, reading the kept turns from the session
    itself rather than from a copy. The pass is branch-scoped: it sizes and
    folds only the ACTIVE branch — what the projector sends — so an abandoned
    attempt neither trips the threshold nor leaks into the summary, and is
    never itself folded (it is already out of the model's view). A pass can
    also be forced by hand (`POST /sessions/:id/compact`, the Context panel's
    "Compact now"): Force skips only the threshold — every other guard holds,
    so a manual pass can never fold what an automatic one would have kept.
    The summarization request carries the folded prefix as ONE plain-text
    transcript under a single user message: the conversation is the summary's
    DATA, not the summary model's history, so no provider's history validation
    (call/output pairing, DeepSeek's reasoning round-trip) can reject the pass.
    The TRANSCRIPT is decoupled from the fold: folded entries render in place,
    in full — history is not what compaction deletes — and the checkpoint
    renders where it sits as an inline "~39k → ~7.8k tokens" marker with the
    summary one expand away. Which entries the model still reads is the
    Context panel's question, never the transcript's: hiding folded turns
    inside the marker made the conversation unreadable past every pass and
    broke everything that reads the rendered timeline (the trace panel's run
    grouping above all).
25. **Schema changes ship without migrations.** `CREATE TABLE / INDEX IF NOT
    EXISTS` is the whole story; a structural change to an existing table means
    dropping and recreating the database (dev-tool stance, decided
    deliberately). Never add ALTER TABLE migration machinery here. The
    no-migration stance is honest only if the mismatch is loud: startup
    probes every model with a zero-row SELECT (bun names every mapped
    column), so an old database file fails fast with a "delete and recreate"
    message instead of surfacing per-request as `no such column` — the
    models themselves are the schema version, no constant to forget to bump.
26. **Where a session stands is stored, not folded.** An append must not cost a
    read of the session: the SDK persists once per TURN, so a run appends many
    times, and folding the branch tip out of the entries each time made one run
    cost O(entries²) — over a log that only grows, since compaction
    soft-deletes. The tip and the highest sequence number live in
    `append_points`, keyed by `(session_id, gen)` like every other address of an
    entry row. It is not a cache: `appendTo`, `Clear`, `pop`, `ForkSession` and
    the compaction adapter's fold each write it inside the transaction that
    carried their change, so the two records cannot come apart, and
    `foldAppendPointIn` stays the definition they must agree with — field for
    field, not "close enough". `TestAppendPointMatchesTheFold` holds the paths a
    session walks in place, `TestForkCutOnAFoldedEntry` the copy a fork makes
    (a fork carries compacted rows too, so a cut landing on one must not make it
    the destination's tip), and
    `TestPersistCompactionParentsTheCheckpointAtTheSurvivingTip` a fold that
    takes the tip with it. A missing row falls back to the fold rather than
    reading as an empty session — that is what a database written before this
    table holds, and calling it empty would make the next append a new root and
    leave the whole conversation on an abandoned branch. `GetEntries` still
    folds the whole session on every call: a known cost, deliberately left,
    because a person pays it once per page while a run paid it once per turn.
27. **A session's `project_id` binding is immutable and server-authoritative**
    — which tree it uses, not how that tree's container is configured: a
    project's environment, and the image on the sandbox it names, are CONTENT
    and stay editable while sessions are bound, reaching them at their next run
    ([decisions §5.32](decisions.md), [§5.36](decisions.md)). A project pins
    its sandbox, so one column is the whole binding. The first
    project-carrying run binds it (`BindProjectIfEmpty`) and nothing changes
    it afterwards: there is no unbind, no rebind, and no PATCH. From then on
    `startRunWithID` overrides whatever the client sends with the bound
    value; the top bar shows the binding as a read-only badge. **A run naming
    no project gets no sandbox tools at all** — no working tree, no file or
    command tools. The write order is what keeps a refusal side-effect-free:
    `planProjectBinding` resolves and validates the project, then
    `hub.register` claims the session slot, and only then does the CAS write
    land — a run refused as busy/deleting/draining has NOT silently fixed the
    session's file system context (`hub.unregister` withdraws a registration
    whose bind failed). The binding's target is protected in both directions,
    atomically: on SQLite the delete refusals live in the delete statements
    themselves and the bind carries the mirror `EXISTS` guard over the project
    row; on PostgreSQL each write locks the parent row first and re-reads its
    guard under the lock ([decisions §5.28](decisions.md)) (the
    operational surface is [Sandboxes](../reference/protocol.md#sandboxes--apiv1sandboxes) and
    [Projects](../reference/protocol.md#projects--apiv1projects)); a project create
    locks the sandbox row so a racing delete refuses the create; and an update
    that would change a SANDBOX's identity — the type and the destination — is
    refused while project rows live on it
    (`UpdateIdentityIfUnreferenced`). A project MOVES only between sandboxes
    that share a destination, checked inside the write's transaction with that
    row locked (`ErrSandboxMoveDestination`). A bound session whose project cannot be
    resolved or built fails the run loudly rather than degrading to a chat
    with no tools. Sandbox instances are cached per
    `(project id, runtime generation)` with a REFERENCE COUNT
    (`SandboxManager.Acquire`): runs and terminals hold their instance for
    exactly their lifetime, and an eviction (a content change anywhere
    upstream, the project's deletion, or the last bound session going away —
    `ReleaseSessionBinding`) closes an idle instance immediately but only
    DOOMS a held one, which the last release closes — an in-flight run or an
    open terminal is never torn off its connection. Task child sessions
    inherit the parent's project through `Inherit` and bind their own hidden
    sessions with it.

28. **Every figure in the Context panel says which ruler it is on, and they are
    never mixed.** `/sessions/:id/context` reports three kinds of number and the
    panel draws them apart. (a) The WINDOW figures — `input_tokens`,
    `cached_tokens`, `cache_write_tokens` — are the provider's own counts for
    the last model call, covering everything it sent: history, system prompt,
    tool schemas. (b) `compaction_tokens` is *exactly what the pass compares* —
    `store.ActiveContextTokens` over the same rows, both sides resolving the
    active branch with the same walk (`activeBranchOfRows`): the most recent
    usage-bearing entry's TOTAL prices the history through itself, plus a
    character estimate for the turns since. A fold NEWER than that pricing
    invalidates it — the priced history included what the fold removed — so the
    figure is fully estimated (kept tail + summary + turns since) until the
    next call re-anchors it; without this the number would hold its pre-fold
    height, and a manual Compact would look like it did nothing. It is therefore mostly a provider
    number, not a character sum — a character sum would draw a threshold line
    that does not match the one that fires. The panel draws ONE bar (the
    window's), with the compaction threshold as a TICK on it and the exact
    comparison (`compaction_tokens / threshold`) as its own indented numbers
    row: the tick marks roughly where the fold lands on the window's scale,
    while the row keeps the comparison on its own ruler — the bar and the
    numbers never pretend to share one. (c) `conversation_tokens` and the
    `prompt` breakdown are character estimates (`CharEstimator`, ~4 chars per
    token): good for shares and for "roughly what does this cost", never for
    arithmetic against (a) or (b) — which is why the "In the window" section's
    percentages are shares of their OWN estimated total, one ruler throughout
    (only the section's header line relates that total to the declared window,
    which is configuration, not a measurement). The UI does not BADGE
    estimates — a badge is skipped by the eye that reads the digits, and four
    exact-looking digits claim a measurement the estimator never made. It
    renders them to the precision they have: two significant figures behind a
    `~`. A figure without one is the provider's own count, which is the whole
    labelling scheme. Sub-agents appear in none of them — a task runs on its own session with
    its own window (invariant 22's Inspector shows that one); what lands in the
    parent's context is the result text, counted like any other tool output.

29. **A workflow execution is a TASK, and it advances from the run's
    TEARDOWN, never from the callback of the run that started it.** A step
    is an ordinary run; when it ends, `postRun` — which every segment
    reaches, fresh or resumed — hands the outcome to the SDK's task manager,
    which asks the driver and only then starts the next step. Hanging the
    advance off the starting call's own callback loses the sequence at the
    first approval: a paused step's run ends, the approval endpoint resumes
    it with NO callback,
    and that resumed ending is the one that may move the sequence on. The
    advance is the SDK's `Store.Advance` — a compare-and-set on
    `(status = working, run_id = the run that finished)` that lands the new
    state in the same write — so a superseded attempt's late callback cannot
    drive it, a stop that got there first wins, and an INTERRUPTED outcome
    advances nothing — it is a pause, not an ending.

30. **A workflow runs OFF the conversation that asked for it, and starts only
    with a BRIEF written by someone who read that conversation.** The steps
    execute on a hidden child session, so a sequence's plan-then-write-then-
    check never enters the chat and no later turn pays for the whole
    procedure; what comes back is the result, through a wake-up. The steps
    still share that one session with each other, so a later step reads what
    an earlier one did — the isolation is between the sequence and the chat,
    not between the steps. It shares the parent's SANDBOX, which is what lets
    the deliverable be files rather than a description of files. The child
    session cannot see the conversation, so someone has to write its brief:
    the agent, through `spawn_task(workflow=name, input)` — a workflow's
    `description`, what the agent matches a request against, is required for
    that — the person, through the manual start in the
    [Workflows API](../reference/protocol.md#workflows--apiv1workflows) — or a trigger, whose author
    wrote the brief in advance for every
    fire (a webhook's payload rides along with it). What there is not is a
    bare button: nothing starts an execution without saying what it is about.
    The tool call's card is the execution's
    card: the task carries the `tool_call_id`, so the sequence's state follows
    the call in the transcript, as a spawned task's does.

31. **An execution's state keeps a log of every step LAUNCHED.** The task row
    names only the CURRENT step and run, so without the log a finished
    sequence could not say which turns belonged to which step, and a retry's
    second attempt could not be told from the first. `state.step_runs` records
    every `(step, run)` the launcher started, written by the launcher itself —
    under the run it is about to start, through the same `Advance` CAS, so it
    lands atomically with the row and a stop racing the launch loses the
    write, not the run. A run that never launched (a crash between the claim
    and the launch) is therefore not in it, which is the truth: the log is of
    launches, and the bounds — the lap bound reads its laps off it, the step
    ceiling (`MaxStepRuns`) counts its entries — count launches, because that
    is what costs. How each run ended is written when the driver decides what
    follows it — the last one IN the finalize itself (an ending
    `Continuation` carries the state into `Store.Finalize`), so the log and
    the task's terminal status are one write and cannot disagree.

32. **Delivery is a DEBT, not a call, and one waker owns it.** Background work
    finishes when its session may be busy, paused on a human decision, or gone
    with the process, so "session S is owed a turn carrying P" is a row
    (`wakeups`), drained at the moments a session becomes able to take one: the
    end of any run on it, and startup. One drain pays every debt the session
    has, so three results that landed while you were typing produce one turn,
    not three. Tasks are its one source — a workflow execution is a task —
    and every debt carries `inherit`, the configuration the turn runs under,
    snapshotted from the agent that ASKED (the spawning run's), so a session
    re-pointed at another agent mid-sequence still returns the result through
    the one that started it. The SDK's task manager keeps no debt of its
    own — it reports endings
    through `OnFinished` and deliveries through `OnResultDelivered`, because
    when a session may be interrupted is a host policy. A task's debt is written
    where its terminal state is: the store's `Finalize` (and the restart sweep's
    `FailOrphans`) records the `wakeups` row IN THE SAME TRANSACTION as the
    status, so a crash can never leave a completed task whose parent is never
    told — a completed/failed task owes, a cancelled one does not (the user
    did it). `OnFinished`
    then only DRAINS (pays now if it can); losing that call loses nothing, since
    the debt already exists and the next boundary settles it. The debt's inherit
    strips the task's own agent (`TaskAgentID`): the drain GROUPS debts by the
    inherit string, and a field the wake run never uses would split one turn
    into one per task agent. A debt born with no agent config is cancelled at
    the first drain — its inherit is frozen, so no boundary will ever do
    better. Wakeup rows match their session by ID ONLY (no generation column,
    unlike task rows — spec §2.13): the session delete cascade removes them in
    the same transaction, and both writers CAS on rows that cascade too, so a
    dead incarnation's debt cannot exist to be inherited. Single insurance,
    recorded deliberately. At startup the two halves run in a fixed order:
    `FailOrphans` first and synchronously, before any request can arrive —
    the sweep fails every row still recorded as working, so a retry that
    slipped in ahead would have its fresh run declared dead — and the drain
    after the handlers are wired, on its own goroutine, because it starts
    runs and the WS handler is what installs `OnRunAttach`.

33. **Plan mode is a restraint, so only a PERSON turns it on, and it belongs to
    the SESSION.** A session executes until somebody asks for a plan — a
    `/plan <message>` in the composer (offered when `/` is typed; nothing arms
    plan mode ahead of a message, the command is the message's; `/plan off
    <message>` is the way back out, an approved plan the other), riding on the
    run request's `plan` field (there is no phase endpoint; setting it and starting the run
    are one step, applied inside the run reservation so a busy-refused request
    never mutates the phase). It is not an agent setting and not something the
    model decides: the value of the gate is "a human looks before anything
    changes", and asking the model whether it needs looking at is asking exactly
    the wrong participant — a model that judges "simple, no plan needed" is the
    failure the gate exists to catch. `plan` absent leaves the phase alone, so a
    client that knows nothing of plan mode cannot knock a session out of it. An
    approved plan unlocks the SESSION rather than the turn, so the next message
    does not demand a second plan for work already agreed. The phase is a
    materialized `sessions.planning` column — read on every run and every
    session GET, so it is one row, not an O(n) scan of the entry log — written
    by the person's request and cleared by the approved `submit_plan` (that
    write is the durable unlock, and its persistence is the precondition for the
    run leaving the planning phase). A fork copies the column, so a branched
    session inherits the phase it forked in. The build is unconditional — every
    chat agent gets the gates and the phase decides whether they bite, because a
    resume rebuilds the agent AFTER the unlock and one built without
    `submit_plan` could not answer for the call the paused state names.

34. **A BACKGROUND run is built without plan mode or the task tools — and is
    TOLD that nobody is reading.** One flag, because
    all of it follows from the same fact: nobody is sitting in front of it.
    Plan mode is the one that deadlocks — `submit_plan` pauses for approval,
    and a background run's approval lands in a session the chat cannot open, so
    the sequence waits forever on a question nobody can see. Removing tools is
    only half of it: an agent still behaves like one in a conversation, asking
    for confirmation and stopping, and in a session nobody reads that is a
    deliverable nobody can answer. So the instructions say so, as a SUFFIX —
    the agent's own prompt may well tell it to ask. A run learns it is
    background from its session being a task's child (a workflow step's is —
    an execution is a task), and a lookup that FAILS is an error, not a "no" —
    reading it as a chat run is exactly how the deadlock happens. A CHAT run
    carries the task tools by default, but an agent may drop them —
    `behavior.subagents: false` — so a chat-only agent that never delegates
    stops paying the `spawn_task`/`task_*` schema on every request; the
    `/workflow` command runs a workflow server-side regardless, so opting out
    never blocks that.

35. **A step's approval is answerable from the conversation that asked.**
    `GET /sessions/:id/approvals` returns the approvals paused inside this
    session's tasks too — a workflow step's included, tagged with the task
    they belong to — so the chat is the one approval surface. Everything else
    about the pause — the reaper, the restart sweep, a stop — is invariant
    37's.

36. **A finished piece of background work leaves the transcript and enters the
    panel.** The Tasks panel holds tasks and workflow executions in ONE
    list — one list because they are one thing, tasks: work running in a
    session nobody is sitting in, reporting back the same way, stopped and
    retried the same way (the notification's rendering rule is invariant
    21's). The list is the socket's task state: the durable rows seed it and
    `task.updated` keeps it current — a workflow's status is the TASK's, told
    by that event, since its step runs end without ending it. Its detail lens
    is the child session's own transcript and trace, so drilling into a
    sequence shows every step in order.

37. **A step a person must approve is a PAUSE of the task, filed as an
    approval — not a run.** Reaching a `pause_before` step, the launcher keeps
    the turn the step will start with in the state, files a pending approval of
    kind `step` under the run id the row already claimed (its one "tool call"
    is `start_step`, naming the step), and marks the task `input_required` —
    the three in ONE transaction, so no moment exists with a task paused on
    nothing to answer or an approval answerable for a task still working. No
    run exists until the decision: approving reclaims the task (the same
    `input_required → working` CAS a tool approval takes, bound to that run
    id) and starts the step's run under it; rejecting stops the execution —
    cancelled, the person's decision, so nobody is woken. A decision is ONE
    transaction as well — the approval row deleted (the exclusive claim) and
    the task moved (`ClaimApprovalWorking` / `ClaimApprovalCancelled`), the
    reaper's expiry included — so of two racing decisions exactly one lands,
    a claim that does not hold writes nothing (the row stays for the retry of
    the decision), and no crash between the two halves can leave a task paused
    on nothing or answered while paused. Because it is a task
    pause and an approval row, everything a tool approval already has applies
    unchanged: it is listed on the parent's approvals and answered from the
    strip, the reaper expires it (cancelling the execution), the restart sweep
    leaves it alone, a stop discards it, and `task.updated` names the decision
    (`pending_call_id`) so a client can offer it without a run event to learn
    it from — and since the pause has no run stream to ride, that event is
    BROADCAST to every connection rather than published on the (nonexistent)
    run; the same fallback covers a step transition or a retry announced
    before its run registers. The one thing it deliberately is not is a `RunState`: there is
    nothing to resume, so the resume machinery never sees it (`Kind` says so
    before the state is read).

38. **The chat's session scope is FOUR contexts, split by how often each
    moves — and what moves per streaming delta is in none of them.** A React
    context has no selectors: every consumer re-renders when its value
    changes, so one context holding the run's `streaming` text would re-render
    every finished turn of a long transcript on every delta. `ChatSessionState`
    (the run lifecycle: flips per run), `ChatActions` (callbacks: change on a
    session switch), `ChatTaskLookups` (the per-call maps a tool card reads:
    identity-stable, moving only when a map's CONTENT changes) and the
    background items (the list the strip, the Tasks panel and the top bar
    show: moves per task event) are memoized on their inputs in `ChatView`;
    `streaming`, `reasoning` and the live turn's `parts` stay props of the ONE
    live `TurnBlock`. The lookups and the items are two contexts, not one,
    because a child run's every tool call patches its task's `lastTool` — an
    event the items must carry and the cards must not see, or every tool card
    of a long transcript would re-render per task event. A test pins it: a
    delta re-renders the live turn and nothing else, and a task event leaves
    the cards alone. This is why the deep components (`TurnBlock` →
    `ProcessTimeline` → `ToolCallCard`, the strip, the Tasks panel) read the
    scope with `useChatSession` / `useChatActions` / `useChatTaskLookups` /
    `useChatBackground` instead of receiving it four levels down.

39. **A workflow definition the model writes lands only through an approved
    `save_workflow`, names steps rather than ids, and never reaches a
    background run.** Two claims. (a) Only an approved save writes. Authoring
    is a WRITE to configuration, so its gate is not the model's to switch
    off: `save_workflow` carries `NeedsApproval` itself (like `submit_plan`),
    not through the agent's `approve_tools`, and its approval card lays the
    proposal out as the server would store it — trimmed, gate words as
    `Verdict` compares them, edges resolved to the step's own spelling — so
    its chart and its diff against the stored definition show the save, not
    the model's spelling of it. The gate's other half is `NeedsApprovalFunc`:
    the same resolve the write does runs before anyone is asked, and a
    proposal that would not save — an unknown agent, a duplicate or reserved
    step name, an edge to no step, no description, a gate whose words
    collide — needs no approval and executes at once into a refusal the
    model reads; only a fixable fault skips the person, a store fault still
    asks, so no write ever lands unapproved. (The pair is per-agent opt-in,
    `behavior.workflow_authoring` — a save schema on every request of every
    agent would be paid for by agents that never author, and writing
    definitions is one agent's job, the builder's — and attached only where
    the task tools are, on a chat run: a background run has nobody to
    approve, and a step that could write definitions would be a sequence
    editing sequences.) (b) The model addresses workflows by NAME; the
    server owns the ids. Steps, agents and edges are NAMED
    (`bridge.workflowSpec`); the server assigns the ids, and on an update
    reuses the id of a step whose name (or, for a nameless one, its `Step N`
    as `get_workflow` reports it) is kept, which is what keeps a retry and
    an execution in flight naming the same step across a model's edit. Same
    name means the same workflow (`EqualFold`, as the unique index is
    `NOCASE`): a save is an upsert, and its result says which it did. Names
    being the handles, the STORE enforces them for every writer
    (`NormalizeWorkflow`: a step name is unique per definition,
    case-insensitively, and `end` is reserved), so a definition the hub
    saved is one the model can read back and edit. (`get_workflow` is
    `ReadOnly`, so plan mode reads definitions and withholds saves; the
    Context panel meters the pair as its own bucket, `tools · workflows`.)

**Configuration**

40. **A global setting is one entry in the registry, and everything else
    derives from it.** `internal/settings` names every key, its kind, its
    default and how the panel presents it; nothing may name a setting any
    other way. The backend reads through `settings.Reader` (which resolves the
    registered default, so no reader carries its own fallback), masking is
    `Kind == secret` rather than a hand-kept list, and the panel renders the
    table the server serves rather than a copy of it. The rule exists because
    the key set used to live in four places at once — backend literals, a
    masking map, the provider table, and a `DEFAULT_KEYS` array in the
    frontend — and `approval_ttl_minutes` is what that cost: read by the
    reaper, documented here, and invisible in the UI until the registry
    named it.
    A default lives in the registry, never in a `const` beside its one reader:
    a default the panel cannot show is one the operator has to read the source
    to learn.
    Presentation derives from the registry too: every bool carries a
    registered default and renders as two states — unset reads as that
    default (`Reader.Bool`), so a third "Default" option would be the same
    answer twice. This is deliberate for `trace_include_sensitive_data` as
    well: the server resolves and passes the value explicitly as
    `Observe.IncludeSensitiveData`, the SDK's one authority for it (the SDK
    reads no environment variable — spec §2.14). The settings panel is the one
    switch, rather than a tri-state control guarding an escape hatch nobody
    could see.
    Bool clicks store immediately (a segmented control reads as applied on
    click), and the control gets an `onChange`: without one Primer's
    `SegmentedControl` is uncontrolled — the selection freezes at first
    render, before the settings fetch resolves, which is how a stored bool
    setting used to show as unset after every refresh.

41. **A destructive action confirms once, in one place.** Every settings
    panel's Delete goes through `useCrud.remove`, which asks (Primer
    `useConfirm`) before calling the API — a new panel gets the guard by
    construction rather than by remembering to add it. Deletes that live
    outside `useCrud` (conversations, skills, background tasks, triggers, the
    unrecognized-settings delete included) use the same `useConfirm` dialog,
    never `window.confirm` and never a bare button. The rule exists because
    the guard used to be per-panel and eight of ten destructive flows had
    none.

42. **Ownership is sessions' owner column, configuration scope, or projects'
    per-user ownership — nothing invents a fourth scheme.** A task's hidden
    session inherits its parent's owner at creation
    (`CreateOptions.ParentID`), a trigger's owner is its session's, an
    approval's is its session's. Configuration is either host-owned and
    admin-written (sandboxes, settings, guardrails, memories) or row-scoped:
    `scope: private | global` for **visibility** and `owner_id` for
    **authorship**, two independent facts — the owner is stamped at create
    and survives every scope flip, changing only through an admin transfer
    (decisions §5.29). Projects are the third, recorded form: per-user working
    trees keyed by (owner, sandbox), with no scope column (decisions §5.28). A new
    per-user thing is filed on a session, takes the scope/owner pair, or —
    when it is a working tree — is a project; never its own column and its
    own checks. Whatever the entity, EVERY mutation re-checks the pair it was
    authorized against as it writes (`409` on a mismatch) — an update against
    the locked row, a scope flip and a delete as a SQL predicate, a skill
    import against its group under lock: a transfer landing between the check
    and the write must never be overwritten by the operation it raced.

43. **Shutdown is ordered, and every waiter is told.** On SIGINT/SIGTERM:
    the clock stops (a tick during the drain would start a run the drain
    refuses); the maintenance loops stop (a reaper ticking during the drain
    could expire the approval being persisted); every running run is
    cancelled and waited for, so its partial turn persists; every run's
    broadcaster is closed — an interrupted run's too, which the drain neither
    cancels nor waits for — so an SSE stream returns (`SubscribeSeq` hands
    every subscriber a `done` channel that closes with the broadcaster,
    retention GC included, and the SSE handler returns on it); the WebSocket
    connections are closed with `1001 Going Away`, since a hijacked
    connection is outside `http.Server.Shutdown`'s reach; then the listener
    drains, for at most five seconds, and whatever it was still waiting on
    is a warning, not an exit status. Only then does the root context end —
    explicitly, ahead of the deferred service and database closes — so
    nothing is torn down under a goroutine still descending from it.

44. **Every per-sandbox operation goes through the backend.** A sandbox's type
    picks the implementation once, in `sandboxes.BackendFor`, and nothing
    outside it branches on the string: the health check behind the Test
    button (`Manager.Check`) and the rebuild
    (`Manager.RebuildContainer`) are `Backend` methods like provisioning is.
    A handler reaching for `DaemonOptions` — the docker daemon's connection —
    is asking a service-managed sandbox for a docker client it does not have,
    which is how `unknown sandbox target type: e2b` reached a person's
    screen; that call now names the sandbox and its type in its refusal, and
    the paths that are genuinely docker-only (Containers, Stop, Remove) refuse
    before starting rather than mid-way. **A rebuild is not universal**: on docker it
    replaces the container and keeps the volume, and on an E2B-compatible
    service — where the sandbox IS the storage
    ([decisions §5.34](decisions.md)) — it is refused with the way out
    (export the project first), never approximated by destroying the working
    tree.

45. **A sandbox is one row, and only its identity freezes.** Where it runs
    and what runs on it live together in `sandboxes`; a project names one
    ([decisions §5.36](decisions.md)). The mutability line is between FIELDS,
    not tables: `SandboxIdentityChanged` covers the type and the destination for
    every backend, plus, for e2b, the fields a `/connect` resume cannot re-apply
    to an already-provisioned instance — `template_id`, `auto_pause`,
    `allow_internet` (editing one while projects live on it is `409`, not a save
    that never takes effect); `timeout_seconds` is exempt, since resume re-sends
    it. Everything else edits freely and reaches bound sessions at their next
    run. There is no separate template entity, and therefore no pair that can
    disagree about its type.

46. **A delete that could not reclaim the storage still deleted the project.**
    `DELETE /projects/{id}` answers `200 {deleted, storage_error?}`: the row is
    gone whenever it answers at all, and a `storage_error` names storage left
    for the operator — never a project that survived. An error STATUS there
    told a client the opposite, and a client that then skipped its refresh
    kept offering a project the server no longer had.

47. **(retired 2026-08-31)** Port preview removed — the workbench no longer
    proxies a sandbox port to the browser (decisions §5.35).

48. **(retired 2026-08-31)** Port preview removed — see invariant 47 and
    decisions §5.35.

49. **The top bar re-reads the sandbox state on the edges that move it.** The
    compute state a project menu shows comes from `GET /projects/:id/sandbox`,
    read when the bound project changes, when the menu opens, AND when a run
    starts or ends — a run's first command starts the container and its end lets
    the idle timer stop it, neither of which notifies the client. A stale read
    never wins a race: each refresh carries a sequence, and only the newest for
    the current project lands, so a slow response for a project just switched
    away from cannot overwrite the current one. A read in flight shows
    "Checking…" only on the FIRST read (no prior value); a failed read leaves
    the last value, or offers Start — never a permanent "Checking…".

50. **Wherever the UI names an agent, `AgentAvatar` draws it.** The agent's
    `avatar` is a path into the built-in catalog shipped with the frontend
    (`public/avatars/`, mirrored by `lib/avatars.ts` and its sync test); the
    server rejects anything else, since the CSP (`img-src 'self'`) would
    render an external URL as a broken image anyway. No avatar renders as the
    name's initial — the same circle `UserAvatar` draws for people — never an
    icon standing in for one agent. Where only a name string crosses the wire,
    the protocol carries the config id beside it (`run.handoff`'s
    `from_id`/`to_id`, the trigger note's `agent_config_id`) and the client
    resolves the avatar from its agent list;
    single-agent pickers render through the shared `AgentPicker`, not a native
    `<select>`, which cannot show an image. Three sizes only — 20px inline
    beside a line of text, 32px where the avatar spans a two-line row (the
    settings list, the picker grid), 56px on the agent form's picker (the
    bare circle right of the Name block, spanning its label and field — the
    avatar IS the button, with a hover pencil badge, never an avatar boxed
    inside a bordered button); the
    one exception is inside a Primer `Label`, whose height caps the avatar
    at 16px.

51. **A tabbed dialog (Settings, Admin) and the Workflows hub keep each panel
    mounted once visited and switch by visibility — never by remount.** A
    remount re-runs the panel's `useApi` from its empty state, which flashed a
    skeleton or blankslate and threw away open-form and scroll state on every
    tab switch — worst in dark mode, where the light placeholder is the most
    visible. So each visited panel stays in the DOM and inactive ones are
    `hidden`. Two things this makes load-bearing: (a) the pure-form column
    narrowing (`:has(.settings-form)`) is scoped to the individual panel, not
    the shared scroll box, or a hidden panel showing a form would narrow the
    visible one; each panel is its own scroller with `scrollbar-gutter: stable`
    so switching never blinks a scrollbar in and out. (b) A kept-but-hidden
    view stands its live work down — `RunsView`'s per-second duration ticker
    gates on an `active` flag — while its event-driven refetch (`tasksSig`)
    keeps running, so returning to it is both instant and current.

52. **A project-bound session transfers only to the project's owner.** The
    binding is immutable (invariant 27), and the session's runs execute in the
    bound project's container — working tree and write-only environment
    included — so `PUT /sessions/{id}/owner` refuses any other target with
    409. Reassigning a session must never be the act that hands one member's
    files and secrets to another; an admin manages the plane, they do not
    redistribute its contents.

53. **A sandbox type's semantics live in the store's kind descriptor; the UI
    offers capabilities from `supports`, never by sniffing `type`.** Every
    per-type answer — content/destination/identity comparison, declared
    ports, the freeze message, the storage hint, the capability flags — comes
    from `sandboxKinds` in store/sandbox.go; a binary type branch anywhere
    else gives a WRONG answer for a third backend, not an error (the inverted
    ports check and the decode-as-the-other-type destination were real).
    Every sandbox API row carries `supports` (`rebuild`), derived per type in
    `sanitizeSandboxConfig` — never stored, never client-writable — and the
    frontend keys Rebuild off it (the per-type CONFIG form in SandboxPanel is
    the one legitimate type switch).
    exec_command advertises `session_id` only when the built sandbox
    implements `TerminalOpener` (spec §2.7k). Adding backend #3 is then: a
    descriptor entry, a `Backend` registry entry, a config struct +
    `NormalizeSandboxConfig` case, secret key names, and a form panel — no
    hunt for scattered branches.

54. **A configuration value lives on exactly one of three planes, and the plane
    is decided by a rule, not by habit.** (1) A **process flag** (cobra, in
    `cmd/root.go`) is for what must be fixed for the process's life, for one of
    two reasons: it is needed before the DB and API exist (`--host`, `--port`,
    `--db`, `--secret-key-file`, `--auth`, `--base-url`, the OAuth bootstrap
    flags); or it is security-load-bearing such that letting a signed-in admin
    mutate it through the API would defeat its purpose (`--token`,
    `--trusted-proxies`, `--audit-retention-days` — the audit log must not be
    shortened through the API it records). (2) An **environment variable**
    exists *only* to keep a secret off argv/`ps`; it is always the env fallback
    of an explicit flag, never a standalone knob — `AGENTS_SECRET_KEY`
    (↔ `--secret-key-file`), `AGENTS_OAUTH_GOOGLE_CLIENT_SECRET`
    (↔ `--oauth-google-client-secret`), `AGENTS_TOKEN` (↔ `--token`), each
    resolved flag-wins-then-env. The server adds no viper-style "every flag is
    also an env var"; that ambient magic is what the SDK's own no-env rule
    (spec §2.14) exists to avoid. (3) Everything an operator tunes live without
    a restart is a **DB setting** in the `settings` registry (invariant 40):
    `proxy_url`, the trace/log toggles, and the caps — `approval_ttl_minutes`,
    `max_tasks_per_session`, `max_terminals_per_sandbox`, `sandbox_idle_minutes`.

    A cap being an input to the embedded SDK is **not** a reason to make it a
    flag: `max_tasks_per_session` feeds `tasks.Config.MaxConcurrentPerParent`,
    which is a `func() int` the manager calls at each spawn — so the server
    backs it with the live setting, both the SDK's spawn gate and the hub's
    register gate read the one resolver, and no enforcement splits. A value only
    forces plane 1 when it is genuinely construction-fixed (a bind address) or
    security-load-bearing, never merely because the SDK consumes it.

55. **A persisted MCP OAuth grant is bound to the config identity it was minted
    under.** An update that moves the endpoint (the grant's audience), the auth
    mode, or the client id clears the stored grant in the same transaction — the
    store preserves the token by copying it forward, so the handler's prepare can
    drop it. A token minted under the previous identity silently authenticating
    the new one is how a swapped OAuth client keeps failing with the old client's
    token while looking freshly configured.

56. **An image attachment is stored as a reference; only the model boundary
    expands it.** Session entries carry `agents-attachment:<id>`, never the
    bucket URL — a `hydratingProvider` wrapped around the run's ModelProvider
    resolves the sentinel against the CURRENT `s3_public_base_url` on every
    request edge (fresh input, replayed history, a resumed RunState, the
    compaction summary), so moving buckets rewires every conversation at once
    and never rewrites history. REST never expands: `GET /messages` ships the
    row facts (`attachments`) beside the entry, and the browser renders those.
    A sentinel whose row is gone degrades to an `[image unavailable]` text
    part — one lost image never takes the conversation with it.

57. **Attachments enter through the composer alone, and leave only by the
    reaper.** `attachment_ids` exists on run creation (REST and WS) and
    nowhere else — task spawns, workflow steps, triggers and mid-run
    injections stay text-only. A run ACCEPTING the ids is what binds them
    (checked ids exist, are the session owner's, are ≤ the per-message cap,
    and the agent's `vision` flag is on — before anything executes); bound
    rows are permanent, surviving session deletion and forks, because a fork
    makes one image answer to two histories. Only never-accepted uploads are
    collected, object before row, after a 24h grace.

58. **The attachment bucket is public-read by design, and the settings save
    proves it.** URLs are stable and unsigned — a presigned URL changes every
    request and with it the prompt prefix, so signing would buy secrecy by
    forfeiting the provider's prompt cache; secrecy rests on unguessable v4
    keys instead. The section saves as ONE group (the Attachment storage
    form; per-key writes are refused) — a change to any field is validated
    with the siblings it will actually be stored with, where per-key saves
    validated a new bucket against the OLD public base and refused the
    change. Every non-empty save (and the form's Test) probes end to end
    (signed upload, ANONYMOUS public read, delete) and refuses on failure,
    because the first symptom otherwise is a model provider's opaque
    download error mid-run; an all-empty save clears the section. The CSP's img-src follows the configured public
    host at runtime (`SetImageHosts` is re-applied on storage-key writes) —
    burned in at startup, a changed bucket shows blank thumbnails until a
    restart.

**Store concurrency**

59. **A write that moves a session's append point takes the session row's
    lock first.** On PostgreSQL every transaction that reads or rewrites a
    session's tip — an append, a clear, a compaction's fold-and-checkpoint —
    opens with `SELECT … FOR UPDATE` on the `sessions` row
    (`EntryStore.lockSessionIn`), and the delete cascade takes the same lock
    before it touches a child table, so the two orders cannot form a cycle.
    Without it READ COMMITTED lets two concurrent appends (a run's persist
    and a task's display update on the parent) read one tip and each link
    to it, and nothing downstream catches the fork: the sequence number comes
    from the clock, not the tip, so the unique index never fires. SQLite's
    single writer serializes by itself, and a direct-scope store has no
    session row and nothing to lock.

60. **An import lands each document in its own savepoint.** A multi-row
    write that reports per-row outcomes (`SkillStore.ApplyImport`) wraps
    every row in `tx.RunInTx` — a `SAVEPOINT` — so a refused row (a name
    collision) is that row's skip and the rows after it still land. On
    PostgreSQL a failed statement otherwise aborts the whole transaction
    (`25P02`), and "one bad file skipped" would silently become "nothing
    imported".

61. **Settings is ONE hub; an admin's views are a toggle inside the same
    panel, never a second dialog.** The gear opens one `PanelDialog`: the
    personal sections (Providers, Agents, MCP servers, Skills, Sandboxes,
    Memory, Guardrails, Account, General) and, for an admin only, an
    "Administration" group in the same nav (Members, Sessions, Projects,
    Workflows, Audit logs). A scoped entity's tab (invariant 42's
    scope/owner rows) carries a "Mine | All members" segmented toggle at the
    top of its body, switching between the personal panel and the
    cross-member management view (`ScopedRowsPanel`: publish/unpublish,
    transfer) — the same rows in one place, not the same entity listed under
    two menu entries. Workflows have no personal settings panel (the sidebar's
    hub is where they are authored and watched), so only their management view
    appears, under Administration. The hub opens from the account menu. The rule exists
    because a separate Admin dialog listed the five scoped entities a second
    time with a different meaning, and a person managing a provider had to
    know which of two identically named tabs to open.
