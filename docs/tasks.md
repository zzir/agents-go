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

`agents/tasks` provides the state machine, the wake-up bookkeeping and the three
tools. What it deliberately does not know is your environment, which arrives
through three injection points.

## Wiring

```go
import "github.com/zzir/agents-go/agents/tasks"

mgr := tasks.New(tasks.Config{
	Store:    tasks.NewInMemoryStore(),   // or sessions.NewTaskStore(db)
	Sessions: repo,                       // an agents.SessionRepo
	Resolver: tasks.AgentResolverFunc(func(ctx context.Context, parentSessionID, name string) (tasks.Spec, error) {
		cfg := lookUpAgent(name)
		return tasks.Spec{DisplayName: cfg.Name, Inherit: cfg.Snapshot()}, nil
	}),
	Launcher: tasks.LauncherFunc(func(ctx context.Context, req tasks.LaunchRequest) error {
		return myHub.Start(req.RunID, req.SessionID, req.Input, req.Inherit)
	}),
	Guard: tasks.AllGuards(notDeleting, noActiveRun, noPendingApproval),
	Stopper: tasks.StopperFunc(func(ctx context.Context, runID string, graceful bool) error {
		return myHub.Cancel(runID, graceful)
	}),
})

agent.Tools = append(agent.Tools, mgr.Tools(nil)...)
```

Then call the Manager at three moments:

```go
mgr.Recover(ctx)                          // at startup
mgr.OnRunFinished(ctx, sessionID, out)    // when ANY run ends
mgr.StopTree(ctx, sessionID)              // before deleting a session
```

`OnRunFinished` is the only entry point that advances state, and it takes every
session — not just task sessions. A parent that was busy while a task finished
has debts waiting, and its own run boundary is where they can finally be paid.

## The three injection points

| | Answers |
|---|---|
| `AgentResolver` | "What is this agent called, and what configuration does it run with?" |
| `Launcher` | "Start a run." (`Wake: true` marks the parent's notification run) |
| `WakeGuard` | "May this parent be woken right now?" |

A `WakeGuard` **must return false when it cannot answer**. A failed query is "I
cannot prove this is safe" — returning true on error makes every outage a source
of spurious runs. `AllGuards` treats a nil guard as a refusal for the same
reason, and a Manager configured with no guard never wakes at all.

## The tools

| | |
|---|---|
| `spawn_task` | Start a task; returns a `task_id` immediately |
| `task_status` | Read one, optionally waiting for it to finish |
| `task_stop` | Cancel one |

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

## Notifications

When a task finishes, the parent is woken with a message carrying every task
that owes it one:

```
[task-notification] Task "index the docs" (a1b2) completed. Result: indexed 412 files… [truncated — call task_status(a1b2) for the full result]
Task "check links" (c3d4) failed. Result: 3 dead links
```

It is a **user-role entry**: the model reads it verbatim, which is the point —
it is news the model has to act on. A UI should detect the
`tasks.NotificationPrefix` and render the message as a notification card rather
than a user bubble.

The notification carries the **summary**, not the result: a task returning ten
thousand words must not paste them into the parent's context to say it is done.
One wake-up carries every pending task, so a dozen finishing together does not
mean a dozen runs each restating the others' news.

## What it guarantees

These are the boundaries the design exists for. Each is a test in
`agents/tasks`.

**Identity.** `Task.ID` and `Task.RunID` are separate: the task is the durable
entity, a run is one attempt at it.

**Finalization is a compare-and-set.** Status, result and the wake-up debt land
in one atomic transition, and only while the task is still non-terminal. Two
finalizers race routinely — a run completing while a stop is in flight — and
without this a terminal state gets overwritten, or `task_status` sees a finished
task whose result has not arrived. This is why tasks require a **transactional
store**; there is no file-backed implementation.

**Four reasons not to wake**, all of which must be clear: the session is being
deleted, it already has a live run, it is paused on a human decision, or the
guard could not tell. A refused wake keeps the debt, and the next run boundary
re-drains it.

**A cancellation never wakes.** The user initiated it, the UI already shows it,
and a wake-up run would only restate it. The same for a result the model already
pulled with `task_status`.

**The wake-up runs under the configuration snapshotted at spawn**, not resolved
fresh: the parent may be configured differently by the time it fires.

**A restart fails what it interrupted.** A task run does not survive the
process, so `Recover` marks still-working tasks failed — which owes each parent
a wake-up, so the news is delivered rather than lost. A task paused on an
approval is left alone: its approval persists and resumes the run.

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
	e, _ := agents.NewUpdateEntry(spawnEntryID, agents.ItemDisplay{
		Extra: map[string]any{"task_status": string(t.Status), "task_summary": t.Summary},
	})
	sess.Append(ctx, e)
},
```

An update entry may be stored **before** its target — a fast task can finish
before the parent turn is saved — and projection associates them by id anyway.
That is what removed the retry loop the original implementation needed.
