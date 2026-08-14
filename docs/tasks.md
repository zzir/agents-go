# Background tasks

A task is a sub-agent that outlives the turn that started it: a tool call spawns
a child run with its own session, the parent does not wait, and the parent is
woken with the result when the child finishes.

```
parent run
  └─ spawn_task(...)  → task id, immediately
     parent finishes its turn
                      ┌──────────────────────────┐
                      │ child run (own session)  │
                      └──────────────────────────┘
                              ↓ finishes
     parent is woken with the result in a later turn
```

`agents/tasks` provides the lifecycle state machine and the four tools. What it
deliberately does not know is your environment — how runs start and stop, and
when (or whether) a parent may be interrupted with a result. The first arrives
through injected functions; the second is entirely yours: the Manager REPORTS
endings, it does not deliver them.

## Wiring

```go
import "github.com/zzir/agents-go/agents/tasks"

mgr := tasks.New(tasks.Config{
	Store:    tasks.NewInMemoryStore(),   // or sessions.NewTaskStore(db)
	Sessions: repo,                       // an session.Repo
	Resolver: func(ctx context.Context, parentSessionID, name string) (tasks.Spec, error) {
		cfg := lookUpAgent(name)
		return tasks.Spec{DisplayName: cfg.Name, Inherit: cfg.Snapshot()}, nil
	},
	Launcher: func(ctx context.Context, req tasks.LaunchRequest) error {
		return myHub.Start(req.RunID, req.SessionID, req.Input, req.Inherit)
	},
	Stopper: func(ctx context.Context, runID string, graceful bool) (tasks.StopOutcome, error) {
		return myHub.Cancel(runID, graceful)
	},
	// The reports. OnFinished: a terminal state was claimed and the parent has
	// not heard — deliver it when YOUR rules say the parent may be interrupted.
	// OnResultDelivered: the model already pulled this result in-turn — drop
	// whatever you recorded to deliver.
	OnFinished:        func(ctx context.Context, t *tasks.Task) { myWaker.Owe(ctx, t) },
	OnResultDelivered: func(ctx context.Context, t *tasks.Task) { myWaker.Cancel(ctx, t) },
})

agent.Tools = append(agent.Tools, mgr.Tools(nil)...)
```

If your host assigns runs identifiers, wrap each run's context with
`tasks.WithParentRunID(ctx, runID)` before starting it: `spawn_task` stamps
that id onto the task (`Task.ParentRunID`), which is what lets a UI tie the
task — and the wake-up run its completion triggers — back to the spawning
run's trace. Display-only; skip it if you have no run ids.

Then call the Manager at three moments:

```go
mgr.Recover(ctx)                          // at startup, BEFORE serving requests
mgr.OnRunFinished(ctx, sessionID, out)    // when a task's run ends
mgr.StopTree(ctx, sessionID)              // before deleting a session
```

`Recover` fails every task recorded as working — a task run does not survive
the process — and reports each through `OnFinished`. It must complete **before**
anything can accept a retry: the sweep has no notion of a live run, so a retry
that got in first would have its fresh run declared dead.

Set `RunOutcome.RunID` if your host identifies its runs. It names the attempt
that finished, so a task retried while that run was in flight keeps the new
attempt rather than being overwritten by the old one's outcome.

## The injection points

| | Answers |
|---|---|
| `AgentResolver` | "What is this agent called, and what configuration does it run with?" |
| `Launcher` | "Start a run." |
| `Stopper` | "Cancel this run" — see below |
| `OnFinished` | Report: a terminal state was claimed; the parent has not heard |
| `OnResultDelivered` | Report: the model pulled this result in-turn |
| `ExtraLiveCount` | The host's OWN background work, counted against the same per-parent cap |

A `Stopper` reports **what it did**, not just whether it errored.
`StopAfterTurn` (still going, will record its own ending — only a graceful stop
may get this) is the one answer that finishes the call. `StopAlreadyFinished`
means it ended before the stop arrived and its outcome is on its way: the stop
waits briefly for that outcome and looks again, since the same answer is what it
hears when a retry has replaced the attempt it was aiming at. If the wait passes
and the row still reads as this attempt, still running, then the outcome was
lost rather than late — and the stop records the ending itself, because a task
nothing will ever end must not also be one nothing can stop. A host that has
never heard of the run says `StopUnknownRun`: a task claims its run before the
launch registers it, so this is a real state, and answering "fine" for having
done nothing is how a stop gets reported as accepted while the task runs on.

The reported `*Task` is the **claimed snapshot in hand** — built from the
finalize's own values, not a re-read. By the time the hook runs, a retry may
already have moved the row past this attempt, and a hook that re-read would see
a working task with the failure cleared.

## The tools

| | |
|---|---|
| `spawn_task` | Start a task; returns a `task_id` immediately |
| `task_status` | Read one, optionally waiting for it to finish |
| `task_retry` | Resume a FAILED one from where it stopped |
| `task_stop` | Cancel one |

`task_retry` starts a new run on the task's existing session, so the model
continues from the progress the failed attempt made instead of paying for it
again. Only a **failed** task can be resumed — a completed one has its answer, a
cancelled one was stopped on purpose — and only up to
`MaxAttemptsPerTask` (default 3, counting the original run). It is a different
job from a model-level retry decorator: that one retries a request the provider
refused, blind to what the run was doing; this one is the parent deciding, with
the failure in front of it, that the work is worth resuming.

`task_status(wait_seconds:)` blocks server-side for up to `MaxStatusWait`
(default 120s). It is one blocked goroutine instead of the model's polling loop,
which is a real token saving. For a finished task it returns the **full** result;
the notification only carried a summary.

A task's own run must not get these tools — that is what bounds recursion. Ask
`MetaFor` before attaching them:

```go
_, isTask, err := mgr.MetaFor(ctx, sessionID)
if err != nil {
	return err // could not tell — withhold the tools rather than guess
}
if !isTask {
	agent.Tools = append(agent.Tools, mgr.Tools(nil)...)
}
```

The error is the point of the third return. A lookup that failed is not the
same answer as "this is not a task": that one hands out the tools, so
collapsing the two would make one transient store error a way past the depth
limit. Refuse instead.

`Spawn` refuses past `MaxDepth` (default 1) as the backstop, and propagates the
same failure for the same reason.

## Delivering results — the host's half

Waking the parent is yours: only the host knows when a session may be
interrupted (not mid-run, not paused on an approval, not mid-delete), and the
SDK owning that policy put it in the wrong place. What the SDK gives you is the
report (`OnFinished`), the addressing (`Task.Inherit`, the configuration
snapshotted at spawn, and `Task.ParentRunID`, the spawning run) and the
formatter (`tasks.DefaultNotifyFormatter`), which renders the message a woken
parent reads:

```
[task-notification] Task "index the docs" (a1b2) completed. Result: indexed 412 files… [truncated — call task_status(a1b2) for the full result]
Task "check links" (c3d4) failed. Result: 3 dead links
(task_retry can resume a failed task from where it stopped)
```

The hint appears when the batch contains a failed task, on a **line of its
own**: a task line is a record consumers parse, and text appended inside one
would be read as part of that task's result. Inject it as a **user-role
entry**: the model reads it verbatim, which is the point — it is news the model
has to act on. A UI should detect the `tasks.NotificationPrefix` and render the
message as a notification card rather than a user bubble. Carry the
**summary**, not the result, and batch every pending result into ONE turn.

A host whose delivery must survive crashes owes itself four rules (spec
"What a durable host owes on top of the reports"):

- Record the debt **atomically with the terminal write** — in your
  `Store.Finalize`/`ReleaseRetryClaim` transaction, not from the hook: a crash
  can fall between the write and `OnFinished`.
- Drop an undelivered debt inside `RetryClaim`'s transition: the task is no
  longer finished, and the next ending owes a fresh one.
- Write each orphan's debt in your restart sweep's own transaction.
- Refuse to wake when you cannot prove it is safe — a busy, paused, deleting
  or unreadable session keeps the debt for the next boundary.

`OnResultDelivered` is the counterpart: the model pulled the result in-turn
(`task_status`, a fast finish, a `task_retry` report), so the recorded debt is
moot. A person reading the same result over a host API has told the model
nothing — never drop the debt on that path.

## What it guarantees

These are the boundaries the design exists for. Each is a test in
`agents/tasks`.

**Identity.** `Task.ID` and `Task.RunID` are separate: the task is the durable
entity, a run is one attempt at it. That separation is what makes `task_retry`
expressible without inventing a second task.

**Finalization is a compare-and-set.** Status and result land in one atomic
transition, and only while the task is still non-terminal. Two finalizers race
routinely — a run completing while a stop is in flight — and without this a
terminal state gets overwritten, or `task_status` sees a finished task whose
result has not arrived. This is why tasks require a **transactional store**;
there is no file-backed implementation.

**A retry is one transition too**, `failed → working`: the new run id, the
attempt count and the cleared summary/result land with the status, only while
the task is failed and under the ceiling. The ceiling is enforced by the store
rather than only by the Manager that checked it, so two processes asking at
once cannot both get an attempt.

**Every finalizer names the attempt it observed.** Since a task can leave a
terminal state (retry), "the row is non-terminal" no longer identifies WHICH
run a writer was looking at — so `Finalize` takes a run id and loses when it is
not the current one. Without that, a stop that read the row just before a retry
would cancel the new attempt while its run kept executing, unkillable, its own
result discarded.

**A retry takes a concurrency slot**, like a spawn: it is a task coming back to
life, and exempting it would make retry the way around
`MaxConcurrentPerParent` (`ExtraLiveCount` adds the host's other background
work to the same cap). If its run fails to start, the task goes back to failed
and the ending follows the model-path rule: the `task_retry` tool reports the
failure in its result (delivered in hand), while a retry over a host API told
only a person — the model still has to hear it, so `OnFinished` fires.

**A cancellation is reported as delivered, never finished.** The user initiated
it, the UI already shows it, and a turn restating it would only repeat them —
so both the stop path and a run reporting a cancelled outcome call
`OnResultDelivered`, not `OnFinished`.

**A restart fails what it interrupted.** A task run does not survive the
process, so `Recover` marks still-working tasks failed and reports each through
`OnFinished` — the parents still have to be told. A task paused on an approval
is left alone: its approval persists and resumes the run.

**A half-finished spawn cleans up after itself**, on a context detached from the
caller's. `Spawn` runs inside the parent run, so a parent cancellation racing it
would otherwise kill the rollback halfway and leave a child session nothing owns.

**`input_required` is not terminal.** A task waiting on a human is still in
flight; delivering a notification for it would announce something that has not
happened.

## Updating the card after the fact

A task's state changes long after the spawning turn ended — that is the whole
difficulty. `Config.OnTaskUpdate` reports each change, and the durable way to
apply it is an [update entry](sessions.md#entries-are-append-only):

```go
OnTaskUpdate: func(ctx context.Context, t *tasks.Task) {
	e, _ := session.NewUpdateEntry(spawnEntryID, agents.ItemDisplay{
		Title:   t.Label,
		Summary: t.Summary,
		Extra:   map[string]any{"task_id": t.ID, "task_status": string(t.Status)},
	})
	sess.Append(ctx, e)
},
```

An update entry may be stored **before** its target — a fast task can finish
before the parent turn is saved — and projection associates them by id anyway.
That is what removed the retry loop the original implementation needed.
