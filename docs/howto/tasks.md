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
deliberately does not know is your environment: how runs start and stop
arrives through injected functions, and when a parent may be interrupted with
a result is entirely yours — the Manager REPORTS endings, it does not deliver
them. A job of SEVERAL runs is a task too, its runs chained by `Continue`
([below](#jobs-of-several-runs); [spec §2.13](../reference/spec.md#213-background-tasks)).

## Wiring

```go
import "github.com/zzir/agents-go/agents/tasks"

mgr := tasks.New(tasks.Config{
	Store:    tasks.NewInMemoryStore(),   // or sessions.NewTaskStore(db)
	Sessions: repo,                       // a session.Repo — see "Deleting a session"
	Resolver: func(ctx context.Context, parentSessionID, name string) (tasks.Spec, error) {
		return tasks.Spec{DisplayName: name, Inherit: lookUpAgent(name).Snapshot()}, nil
	},
	Launcher: func(ctx context.Context, req tasks.LaunchRequest) error {
		return myHub.Start(req.RunID, req.SessionID, req.Input, req.Inherit)
	},
	Stopper: myHub.Cancel, // func(ctx, runID string, graceful bool) (tasks.StopOutcome, error)
	OnFinished:        func(ctx context.Context, t *tasks.Task) { myWaker.Owe(ctx, t) },    // deliver when YOUR rules allow
	OnResultDelivered: func(ctx context.Context, t *tasks.Task) { myWaker.Cancel(ctx, t) }, // the model already has it
})

agent.Tools = append(agent.Tools, mgr.Tools(nil)...)
```

A complete host — agents as a map, launching as a goroutine, waking whenever
the parent is idle — is [examples/tasks](../../examples/tasks/main.go).

If your host assigns runs identifiers, wrap each run's context with
`tasks.WithParentRunID(ctx, runID)` before starting it: `spawn_task` stamps
that id onto `Task.ParentRunID`, which is what lets a UI tie the task — and the
wake-up run its completion triggers — back to the spawning run's trace.
Display-only; skip it if you have no run ids.

Then call the Manager at three moments:

```go
mgr.FailOrphans(ctx)                      // at startup, BEFORE serving requests
mgr.OnRunFinished(ctx, sessionID, out)    // when a task's run ends
mgr.StopTree(ctx, sessionID)              // before deleting a session — then delete the tree, see below
```

`FailOrphans` fails every task recorded as working — a task run does not
survive the process — and reports each through `OnFinished`. It must complete
**before** anything can accept a retry: the sweep has no notion of a live run,
so a retry that got in first would have its fresh run declared dead.

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
| `Continue` | "Does this run's ending end the task, or is there a next run?" — see below |

A `Stopper` reports **what it did**, not just whether it errored:
`StopAfterTurn` (still going, will record its own ending), `StopAlreadyFinished`
(its outcome is on its way — the stop waits briefly, and records the ending
itself if the outcome was lost rather than late) or `StopUnknownRun` (a real
state: a task claims its run before the launch registers it). Answering "fine"
for having done nothing is how a stop gets reported as accepted while the task
runs on. The `*Task` a report hands you is the claimed snapshot, not a re-read
([spec §2.13](../reference/spec.md#213-background-tasks)).

## Jobs of several runs

A task's work may span runs in sequence. `Kind` (what sort of job) and `State`
(where it stands) are the host's own and opaque to the SDK — set at spawn
(`SpawnRequest.Kind`/`State`) and handed to the `Launcher` with every run.
`Config.Continue` is asked when a run of the CURRENT attempt completes or fails
— never when it is cancelled:

```go
Continue: func(ctx context.Context, t *tasks.Task, out tasks.RunOutcome) (*tasks.Continuation, error) {
	if t.Kind != "sequence" {
		return nil, nil                    // an ordinary task: its run's ending is its own
	}
	seq := decode(t.State)
	next, ok := seq.After(out)             // the host's rule — edges, a check, a counter
	if !ok {
		return nil, nil                    // ends with the run's outcome
	}
	if seq.Launches >= 50 {
		return nil, errors.New("looping")  // ends FAILED, with this reason
	}
	return &tasks.Continuation{Input: next.Prompt, State: seq.With(next).Encode()}, nil
},
```

A `Continuation` with an `Input` moves the task on through `Store.Advance` — run
id and `State` replaced in one compare-and-set, only while the task is still
working on the run that just ended; without an `Input` it ends the task, with
`State` written alongside the ending; `nil` ends it with `State` untouched. A
failed launch, a lost transition or an error from the hook ends the task failed
([spec §2.13](../reference/spec.md#213-background-tasks)).
`Config.MaxContinuations` (default 50) caps how many further runs the hook may
chain since the spawn or the last retry — the counter in the example is the
host's own bound, not the only one.

What the host gets: one lifecycle for every kind of background work — stop
chases the current run, retry re-launches the current `State`
(`LaunchRequest.Retry`, its `Input` the retry prompt), the restart sweep fails
the row at the step it reached, and `task_status` and the wake-up report the
task, not its runs. `Task.Kind` is in `task_status`'s output; the SDK never
branches on it.

## The tools

| | |
|---|---|
| `spawn_task` | Start a task; returns a `task_id` immediately |
| `task_status` | Read one, optionally waiting for it to finish; with no id, list the conversation's tasks |
| `task_retry` | Resume a FAILED one from where it stopped |
| `task_stop` | Cancel one |

`task_retry` starts a new run on the task's existing session, so the model
continues from the progress the failed attempt made. Only a **failed** task can
be resumed, and only up to `MaxAttemptsPerTask` (default 3, counting the
original run) — the parent deciding, with the failure in front of it, that the
work is worth resuming.

`task_status(wait_seconds:)` blocks server-side for up to `MaxStatusWait`
(default 120s) — one blocked goroutine instead of a polling loop — and returns
the **full** result where the notification carried a summary. With an empty
`task_id` it lists the conversation's tasks, newest first, each live one
flagged "still working — do not redo its work" (the way back to an id a
compaction dropped; a listing settles no wake-up debt).
`Config.DescribeState(kind, state) string` — "step 2/3 (verify)" — is shown as
`progress:` beside the status.

Four verbs are the whole model-facing surface, whatever the kind: a host that
starts jobs by name provides its own spawn tool from the public parts
(`Manager.Spawn`, `Manager.ModelHasResult`, `tasks.ToolResult`) and attaches
`TaskTools` beside it ([spec §2.13](../reference/spec.md#213-background-tasks)).

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

A lookup that failed is not "this is not a task" — collapsing the two would make
one transient store error a way past the depth limit. `Spawn` refuses past
`MaxDepth` (default 1) as the backstop.

## Delivering results — the host's half

Waking the parent is yours — the Manager reports endings, it does not deliver
them. What the SDK gives you is the report (`OnFinished`), the addressing
(`Task.Inherit`, the configuration snapshotted at spawn, and `Task.ParentRunID`,
the spawning run) and the formatter (`tasks.DefaultNotifyFormatter`), which
renders the message a woken parent reads:

```
[task-notification] Task "index the docs" (a1b2) completed. Result: indexed 412 files… [truncated — call task_status(a1b2) for the full result]
Task "check links" (c3d4) failed. Result: 3 dead links
(task_retry can resume a failed task from where it stopped)
(Tell the person what happened. The work above is done — do not repeat or re-check it unless they ask.)
```

The retry hint appears when the batch contains a failed task, and the closing
guidance always, each on a line of its own (a task line is a record consumers
parse). Inject it as a **user-role entry** — it is news the model has to act
on — and have the UI detect `tasks.NotificationPrefix` to render it as a
notification card rather than a user bubble. Carry the **summary**, not the
result, and batch every pending result into ONE turn.

A host whose delivery must survive crashes writes its own debt row atomically
with `Store.Finalize` / `ReleaseRetryClaim`, drops it inside `RetryClaim`'s
transition, writes each orphan's debt in its restart sweep, and refuses to wake
a session it cannot prove is safe to interrupt — "What a durable host owes" in
[spec §2.13](../reference/spec.md#213-background-tasks).

`OnResultDelivered` is the counterpart: the model pulled the result in-turn
(`task_status`, a fast finish, a `task_retry` report), so the recorded debt is
moot. A person reading the same result over a host API has told the model
nothing — never drop the debt on that path.

## What it guarantees

Every invariant the design exists for — identity separate from execution,
finalization and retry as compare-and-set, a cancellation reported as delivered,
a restart failing what it interrupted — is
[spec §2.13](../reference/spec.md#213-background-tasks), each a test in `agents/tasks`.

## Deleting a session

`StopTree` stops the tasks; it deletes nothing, and neither does the Manager.
What must go with a deleted parent is the whole tree — its task rows (a
survivor owes a wake-up to a conversation that no longer exists) and the hidden
sessions its tasks ran in (unreachable forever once left behind). **That
cascade belongs to the `session.Repo` you pass, on `Delete`**, and only a repo
that holds both tables can do it:

| Repo | On `Delete` of a parent |
|---|---|
| `sessions.NewRepo(db)` (SQL) | Removes the session, its entries, its task rows in both roles, and every hidden session in the task tree, at any depth — one transaction |
| `session.NewInMemoryRepo()` | Removes the session and its entries only — it has no task table to look in. With `sessions.NewTaskStore(db)` beside it the rows and the child sessions are yours to remove: `StopTree`, then `ListByParent` and delete each child, then the rows |

The generation columns on the SQL task store make a row that survives a delete
some other way inert; only the cascade stops it existing.

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
