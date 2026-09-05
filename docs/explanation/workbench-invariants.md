# Workbench design invariants

The cross-panel and cross-handler rules of `agents-server`: what a change in
one place can break in another. Each numbered entry is a permanent address
cited from code as `invariant N`, and holds at most six lines — the rule, its
boundary, and where the reasoning ([decisions](decisions.md) §5.x) or the
mechanism (a file) lives; the SDK's rules are in the [spec](../reference/spec.md).

---

**API shape**

1. **`config` blobs travel as JSON objects, never strings.** Every
   backend-specific settings blob (`mcp_servers.config`, `sandboxes.config`,
   `guardrails.config`) is a `json.RawMessage` exchanged inline; the frontend
   reads and writes it as an object, never `JSON.stringify`/`parse` of the
   field itself.
2. **List responses carry every field the edit form needs.** `useCrud` panels
   initialize the edit form from the list item, so a list-side projection that
   drops fields makes the next save silently wipe them. Return full rows from
   List, or make the panel fetch Get before editing.
3. **Derived state is computed in one backend function; the frontend renders
   it verbatim.** A lifecycle is one server-derived `status` (MCP: `disabled |
   connecting | authorizing | needs_auth | disconnected | connected`) or
   boolean (`chatgpt_logged_in`, `has_oauth_token`). The frontend never
   reconstructs state from several fields or its own per-item maps.
4. **Swagger annotations match the actual response type.** Run `make openapi`
   after any handler change — CI diffs the generated spec.

**State & lifecycle**

5. **An off switch holds at every entrance.** A resource with `enabled=false`
   is refused by every path that could activate it — manual connect, agent
   assembly, startup auto-connect. Agents pick MCP tools by live connection,
   so one unguarded path voids the whole switch.
6. **Create and Update trigger the same side effects.** If updating a resource
   reconciles a live connection, creating one does too.
7. **Async settling uses grace-window polling, not per-item timers.** After a
   mutation that completes in the background (reconnect, OAuth), the panel
   polls the list while any row is transitional or an ~8s grace window is
   open, then stops (`McpServerPanel.tsx`). A one-shot notification (popup
   `postMessage`) only triggers an immediate reload and never owns cleanup,
   because it does not always arrive.
8. **In-progress buttons stay retryable when the wait is on an external
   actor.** A button whose completion depends on the user finishing a popup
   allows a superseding retry (cancel the stale attempt, start fresh) rather
   than disabling itself until a timeout.

**Secrets**

9. **Secrets are write-only and go through `handler/secrets.go`.** Reads mask
   with `********`; writes resolve the sentinel (mask = keep, `""` = clear,
   else replace) via the shared helpers, with a round-trip test. A mask never
   survives a destination change (a changed `provider_type`/`base_url` is
   rejected; a fallback entry restores only on an exact match), and it resolves
   inside the store's transaction or under `expected_revision` — never by a `Get`.
10. **OAuth-class tokens never leave the server.** Own column with `json:"-"`,
    excluded from CRUD updates (`ExcludeColumn`), exposed only as a derived
    boolean (`has_oauth_token`, `chatgpt_logged_in`). A masked token string is
    never a truthiness signal.
11. **An OAuth grant persists as a self-contained refreshable unit, through
    one writer.** The payload carries the token and its refresh context (token
    endpoint, client credentials), and every mutation — the initial authorize,
    every refresh, a rotated refresh token — goes through
    `mcpservers.persistGrant`. Restored and live connections use the same
    refreshing `oauth2.TokenSource`, never a static snapshot of the token.

**Store layer**

12. **No bun `default:` tags on booleans.** bun swaps a zero-value field for
    SQL `DEFAULT` on insert, so `default:true` silently enables a row created
    with `enabled=false`. Use `notnull` and set the value in Go.
13. **Deleting a referenced resource fails loud at use, never silently skips a
    safety feature.** Guardrail names that no longer resolve fail the agent
    build; dangling MCP/skill ids are filtered with a visible count in the UI.
    A new reference picks one of those two behaviors deliberately.

**Chat / run streaming**

14. **Run events are a broadcast bus per owner, not a reply channel.** Every
    WS connection of the session's owner is attached to the run's stream (on
    connect, and via `Runner.OnRunAttach`); `run.started` carries the prompt
    so any of them can render it. Never wire an event to "the connection that
    asked". A late joiner can only place a run it saw start, so the hub pins each
    run's latest `run.started` outside the replay ring (`RunHub.SubscribeSeq`).
15. **Protocol constants have one definition per side.** Event types and error
    codes live in `internal/protocol` (Go) and `src/lib/protocol.ts` (TS
    mirror); emitters and consumers use the constants, never string literals.
    The mirror stops at the WS contract: the REST error envelope
    (`protocol/apierror.go`) is Go-only, since its one browser consumer reads
    `error.message` and never branches on the code.
16. **A streamed turn must equal its reload.** The streaming path
    (`streamReducer.ts` via `useAgentSocket`) and the replay path
    (`buildTimeline` over persisted entries) produce the same `turn.parts`;
    `timeline.test.ts` pins it, and intentional differences are asserted
    there. A new part type or field lands on both paths plus `timeline.ts`.
17. **Terminal run events reconcile against the store.** Every terminal
    handler (output/error/cancelled) applies its optimistic parts, then
    reloads the persisted timeline as the authority. The one exception is
    `guardrail_tripwire`, which keeps the retracted-answer view the SDK never
    persists; a new exception is listed here.
18. **The streaming block patches the DOM; user intent beats the pin.** Live
    text is morphdom-patched, never rewritten via innerHTML — node identity is
    what keeps a selection alive across deltas (`StreamingMarkdown.tsx`).
    Bottom-following re-fires on content growth and yields to an upward
    wheel/drag or an actively changing selection; a stale selection never
    blocks re-sticking (`useScrollToBottom` in `lib/hooks.ts`).
19. **A branch move obsoletes every client view of the old path.** Regenerate
    and attempt-switch are server-side appends (`POST /sessions/:id/branch`)
    and the client reconciles by refetch: the `on_path === false` filter
    applies before any fork exists, a move bumps the timeline generation so an
    older fetch is dropped, the live tail re-appends only the current run, and
    an off-path pending approval stays out of view without losing its row. A
    branch move is refused (`409`) while a run is live on the session — a
    switch mid-run would graft the run's later turns onto the new branch.

**Background tasks**

20. **A task is a durable entity; a run is one execution of it.** The row
    carries `run_id` (the current attempt); events carry `task_id` and
    `attempt`, so clients route by run id and key task state by task id. The
    transcript lives in a hidden child session; the spawn target is an agent
    config by name (`task_runner.go`). Task events use the chat runs' bus,
    cursors, approval persistence and retention — never their own transport.
21. **The spawn card's durable truth is an appended update entry.** A state
    change APPENDS an update addressed to the spawn call's id; the read folds
    it in — non-empty fields, terminal only, never backwards (`task_env.go`,
    `entry_store.go`; live mirror `syncTaskCard`). Status never comes from the
    hub after the fact. The parent wakes through a `[task-notification] `
    user-role input that never renders and carries no privileged behavior.
22. **The right side panel is a single-instance Inspector.** Traces, context
    usage, the task list and one task's detail are lenses of one panel; a new
    surface is a new lens. A span's summary loads with the session, its
    payload only when expanded (`loadSpanPayload`); task detail tails live
    events only while open. Run lineage is `trace_events.parent_run_id`, never
    derived from task rows, notification text or the rendered timeline.
23. **A task's terminal state is written exactly once, via row CAS**
    (decisions §5.54). `Finalize` wins only while the row is non-terminal,
    stop and approve race through the same CAS (`Finalize` vs
    `ReclaimWorking`), and `task_status` trusts only the row — a hub-terminal
    run whose row has not landed is still `working`. A new publisher of
    terminal events gets an atomic hub transition, never a third compensation.
24. **One entry in, the same entry out.** `entries` stores whole
    `session.Entry` JSON, only query columns lifted out; the server never
    re-derives a display, role or provenance at read time. Compaction
    soft-deletes, appends a checkpoint naming what it folded, and sizes only
    the active branch (`compaction_adapter.go`); the timeline stays decoupled
    from the fold — folded entries render in full, the checkpoint inline.
25. **Schema changes ship without migrations.** `CREATE TABLE / INDEX IF NOT
    EXISTS` is the whole story; a structural change means dropping and
    recreating the database, and ALTER TABLE machinery is never added.
    Startup probes every model with a zero-row SELECT, so an old database
    fails fast with a "delete and recreate" message — the models are the
    schema version.
26. **Where a session stands is stored, not folded.** The branch tip and
    highest sequence live in `append_points`, written inside the transaction
    that moved them (`appendTo`, `Clear`, `pop`, `ForkSession`, the compaction
    fold); `foldAppendPointIn` is the definition they must agree with, field
    for field. A missing row falls back to the fold, never to "empty". Only
    `GetEntries` still folds, once per page.
27. **A session's `project_id` binding is immutable and server-authoritative.**
    The first project-carrying run binds it (`BindProjectIfEmpty`): no unbind,
    rebind or PATCH, the run overrides what the client sends, and bind and
    delete guard each other atomically per dialect (`bridge/binding.go`). It
    binds WHICH tree, not the container's configuration: the project's
    environment (write-only, decisions §5.32) and the sandbox's image are
    content, editable while bound, reaching sessions at their next run. No
    project ⇒ no sandbox tools at all. Instances are cached per `(project,
    runtime generation)` — the one fence, bumped by any content change — and
    reference-counted (`SandboxManager.Acquire`): an eviction closes an idle
    instance and only dooms a held one. A project delete destroys its volume
    (decisions §5.33); task child sessions inherit the parent's project.
    A container found stopped is restarted in place; remove-and-recreate only
    when the start fails or the container is gone. The expired/gone fence is
    the one shape for every stop (idle, user Stop, deferred last release): the
    instance keeps its cache key until `Lifecycle.Stop` returns, and a
    deferred stop that new work overtook is superseded. On PostgreSQL a bind
    takes `FOR KEY SHARE` on the project row, a project delete `FOR UPDATE` on
    its own row, sandbox-guarded writes `FOR UPDATE` on the sandbox row, each
    re-evaluating its guard under the lock.
28. **Every figure in the Context panel says which ruler it is on, and they
    are never mixed.** `/sessions/:id/context` reports the provider's window
    counts for the last call; `compaction_tokens`, what the pass compares
    (`ActiveContextTokens`); and character estimates for the conversation and
    prompt, never for arithmetic against the others. The panel draws one bar
    with the threshold as a tick, and an estimate as two figures behind `~`.
29. **A workflow execution is a task, advanced from the run's teardown, never
    from the starting call's callback.** A step is an ordinary run; `postRun`
    — reached by every segment, fresh or resumed — hands the outcome to the
    SDK task manager, which asks the driver (`bridge/workflow.go`). The advance
    is `Store.Advance`, a CAS on `(status = working, run_id)`: a superseded
    attempt cannot drive it, and an interrupted outcome (a pause) moves nothing.
30. **A workflow runs off the conversation that asked for it, and starts only
    with a brief written by someone who read that conversation.** Steps run
    on a hidden child session sharing the parent's sandbox; the result comes
    back through a wake-up. The brief comes from the agent (`spawn_task`,
    matched on a required `description`), the person (the manual start) or a
    trigger's author — never a bare button. The call's card is the execution's.
31. **An execution's state logs every step launched.** `state.step_runs`
    records each `(step, run)` the launcher started, written under the same
    `Advance` CAS as the row; the lap bound and `MaxStepRuns` count launches,
    and an ending's outcome lands in the `Finalize` write so the log and the
    terminal status cannot disagree (`store/workflow.go`).
32. **Delivery is a debt, not a call, and one waker owns it.** "Session S is
    owed a turn carrying P" is a `wakeups` row, written in the transaction
    that lands the task's terminal status (a cancelled task owes nothing) and
    drained — `OnFinished` only drains — when the session can take a turn: the
    end of any run on it, and startup, one turn paying every same-`inherit`
    debt (`bridge/waker.go`). Startup runs `FailOrphans` before any request.
33. **Plan mode is a restraint, so only a person turns it on, and it belongs
    to the session.** The switch is the run request's `plan` field (`/plan
    <message>`, `/plan off <message>`), applied inside the run reservation;
    absent leaves the phase alone. Never an agent setting, never the model's
    decision. The phase is the materialized `sessions.planning` column,
    cleared by an approved `submit_plan`, copied by a fork (decisions §5.53).
34. **A background run is built without plan mode or the task tools — and is
    told that nobody is reading.** `submit_plan` would pause on an approval
    nobody can see, so a background run gets neither, plus
    `BackgroundInstructions` as a suffix (`bridge/agent.go`). Background means
    the session is a task's child, and a lookup that FAILS is an error, not
    "no". A chat run drops the task tools only via `behavior.subagents: false`.
35. **A step's approval is answerable from the conversation that asked.**
    `GET /sessions/:id/approvals` includes the approvals paused inside this
    session's tasks, tagged with their task, so the chat is the one approval
    surface. The pause itself is invariant 37's.
36. **A finished piece of background work leaves the transcript and enters
    the panel.** The Tasks panel holds tasks and workflow executions in one
    list, seeded by the durable rows and kept current by `task.updated` (a
    workflow's status is the task's); its detail lens is the child session's
    transcript and trace (`BackgroundPanel.tsx`).
37. **A step a person must approve is a pause of the task, filed as an
    approval — not a run.** A `pause_before` step files a pending approval of
    kind `step` under the claimed run id and marks the task `input_required`
    in one transaction; a decision is one transaction too (`ClaimApproval*`),
    so racing decisions cannot both land. Listing, the reaper and a stop apply
    unchanged; `task.updated` is broadcast (no run stream). Never a `RunState`.
38. **The chat's session scope is four contexts, split by how often each
    moves — and what moves per streaming delta is in none of them.**
    `ChatSessionState`, `ChatActions`, `ChatTaskLookups` and the background
    items are separate contexts (`ChatSessionContext.tsx`); `streaming`,
    `reasoning` and the live turn's `parts` stay props of the one live
    `TurnBlock`. Deep components use the `useChat*` hooks, never passed props.
39. **A workflow definition the model writes lands only through an approved
    `save_workflow`, names steps rather than ids, and never reaches a
    background run.** `save_workflow` carries `NeedsApproval` itself, not via
    `approve_tools`; `NeedsApprovalFunc` runs the write's own resolve, so an
    unsaveable proposal executes into a refusal and a store fault still asks.
    Per-agent opt-in (`behavior.workflow_authoring`); chat-only (`bridge/agent.go`).

**Configuration**

40. **A global setting is one entry in the registry, and everything else
    derives from it.** `internal/settings` names every key, kind, default and
    presentation; the backend reads through `settings.Reader` (no reader has
    its own fallback), masking is `Kind == secret`, and the panel renders the
    served table. Every bool has a registered default and two states (unset =
    default), stored on click (`SettingsPanel.tsx`); no env fallback (spec §2.14).
41. **A destructive action confirms once, in one place.** Every Delete goes
    through `useCrud.remove` or the same Primer `useConfirm` dialog
    (conversations, skills, tasks, triggers, unrecognized settings) — never
    `window.confirm`, never a bare button.
42. **Ownership is sessions' owner column, configuration scope, or projects'
    per-user ownership — nothing invents a fourth scheme.** A hidden task
    session, a trigger or an approval takes its session's owner; configuration
    is host-owned or row-scoped — `scope` for visibility, `owner_id` for
    authorship, two independent facts (decisions §5.29); a working tree is a
    project (decisions §5.28). Every mutation re-checks the pair as it writes
    (`409`); the matrix is in [protocol.md](../reference/protocol.md#authorization).
43. **Shutdown is ordered, and every waiter is told.** The clock stops, then
    the maintenance loops, then every run is cancelled and waited for (its
    partial turn persists), every broadcaster is closed so SSE streams
    return, WS connections get `1001 Going Away`, and the listener drains for
    at most five seconds — a warning, not an exit status. The root context
    ends last, ahead of the deferred closes (`cmd/root.go`).
44. **Every per-sandbox operation goes through the backend.**
    `sandboxes.BackendFor` picks the implementation once; nothing else
    branches on the type string. Health check and rebuild are `Backend`
    methods; docker-only paths (Containers, Stop, Remove) refuse up front,
    naming the sandbox and its type. A rebuild is not universal: on E2B, where
    the sandbox is the storage, it is refused with the way out (decisions §5.34).
45. **A sandbox is one row, and only its identity freezes.**
    `SandboxIdentityChanged` covers the type and the destination, plus for
    e2b the fields a `/connect` resume cannot re-apply (`template_id`,
    `auto_pause`, `allow_internet`); editing one while projects live on it is
    `409`. Everything else edits freely and reaches bound sessions at their
    next run. There is no template entity — decisions §5.36.
46. **A delete that could not reclaim the storage still deleted the project.**
    `DELETE /projects/{id}` answers `200 {deleted, storage_error?}`: the row
    is gone whenever it answers, and `storage_error` names storage left for
    the operator — never an error status for a row that is gone.
47. **Retired 2026-08-31 with port previews** — decisions §5.35.
48. **Retired 2026-08-31 with port previews** — decisions §5.35.
49. **The top bar re-reads the sandbox state on the edges that move it.**
    `GET /projects/:id/sandbox` is read when the bound project changes, when
    the menu opens, and when a run starts or ends. Each refresh carries a
    sequence and only the newest for the current project lands; "Checking…"
    shows only on the first read, and a failed read keeps the last value or
    offers Start (`ProjectControls.tsx`, `ChatTopBar.tsx`).
50. **Wherever the UI names an agent, `AgentAvatar` draws it.** `avatar` is a
    path into the built-in catalog (`public/avatars/`, mirrored by
    `lib/avatars.ts`); the server rejects anything else. No avatar renders as
    the name's initial, never an icon; where only a name crosses the wire the
    protocol carries the config id beside it, and pickers use `AgentPicker`.
    Sizes: 20px inline, 32px on a two-line row, 56px on the agent form.
51. **A tabbed dialog (Settings, Admin) and the Workflows hub keep each panel
    mounted once visited and switch by visibility — never by remount.** A
    remount re-runs `useApi` from empty and flashes a skeleton. Inactive
    panels are `hidden`, the `:has(.settings-form)` narrowing is scoped per
    panel, each panel scrolls itself with `scrollbar-gutter: stable`, and a
    hidden view stands its live work down (`RunsView`'s ticker; `PanelDialog`).
52. **A project-bound session transfers only to the project's owner.**
    `PUT /sessions/{id}/owner` refuses any other target with 409: the
    session's runs execute in the bound project's container, working tree
    and environment included, and reassigning a session must never hand one
    member's files and secrets to another.
53. **A sandbox type's semantics live in the store's kind descriptor; the UI
    offers capabilities from `supports`, never by sniffing `type`.** Every
    per-type answer comes from `sandboxKinds` (`store/sandbox.go`); a binary
    branch elsewhere is a wrong answer for a third backend. Every API row
    carries `supports`, derived and never stored; the per-type config form is
    the one legitimate type switch.
54. **A configuration value lives on exactly one of three planes, and the
    plane is decided by a rule.** A process flag is for what is needed before
    the DB and API exist or is security-load-bearing (`--token`,
    `--trusted-proxies`, `--audit-retention-days`); an environment variable is
    only a flag's fallback that keeps a secret off argv, never a standalone
    knob; everything tuned live is a DB setting (invariant 40), a cap the SDK
    consumes included. Tables: [configuration](../reference/configuration.md).
55. **A persisted MCP OAuth grant is bound to the config identity it was
    minted under.** An update that moves the endpoint, the auth mode or the
    client id clears the stored grant in the same transaction; a token minted
    under the previous identity must never silently authenticate the new one.
56. **An image attachment is stored as a reference; only the model boundary
    expands it.** Entries carry `agents-attachment:<id>`; a
    `hydratingProvider` around the run's ModelProvider resolves it against
    the current `s3_public_base_url` on every request edge, REST never
    expands, and a missing row degrades to `[image unavailable]` —
    decisions §5.42.
57. **Attachments enter through the composer alone, and leave only by the
    reaper.** `attachment_ids` exists on run creation (REST and WS) and
    nowhere else. A run accepting the ids binds them (owner, cap, the agent's
    `vision` flag — checked before anything executes); bound rows are
    permanent across session deletion and forks. Only never-accepted uploads
    are collected, object before row, after a 24h grace.
58. **The attachment bucket is public-read by design, and the settings save
    proves it.** URLs are stable and unsigned (decisions §5.42). The section
    saves as one group; every non-empty save and Test probes end to end
    (signed upload, anonymous read, delete) and refuses on failure; an
    all-empty save clears it. The CSP's img-src follows the configured host
    at runtime (`SetImageHosts`, re-applied on storage-key writes).

**Store concurrency**

59. **A write that moves a session's append point takes the session row's
    lock first.** On PostgreSQL every transaction that reads or rewrites a tip
    opens with `SELECT … FOR UPDATE` on `sessions` (`EntryStore.lockSessionIn`),
    and the delete cascade takes the same lock before touching a child table,
    so the two cannot form a cycle. SQLite's single writer serializes by
    itself.
60. **An import lands each document in its own savepoint.** A multi-row write
    reporting per-row outcomes (`SkillStore.ApplyImport`) wraps every row in
    `tx.RunInTx`, so a refused row is that row's skip; on PostgreSQL a failed
    statement otherwise aborts the whole transaction (`25P02`).
61. **Settings is one hub; an admin's views are a toggle inside the same
    panel, never a second dialog.** One `PanelDialog`: the person's own
    sections, then what runs are built from, then an admin's management
    entries after a divider. A scoped entity's tab is one list — a member's
    own and published rows, every member's for an admin, "Mine | All" only
    narrowing — managed from the row's menu; only Workflows keep a table. The
    dialog is reachable at `#/settings/:tab`, a one-shot deep link consumed on
    open: the URL keeps naming the view underneath, so a reload never loses it.
62. **A span's payload is content-addressed per session, and lives and dies
    with the session's trace.** Payload elements are stored once per session
    in `trace_blobs` under their sha256, so delete, fork and retention are
    whole-session operations with no reference count (decisions §5.50). Two
    retention layers: row retention drops a rowless session's blobs;
    `trace_payload_retention_days` strips an idle session's (nulling `layout`/`refs`).
