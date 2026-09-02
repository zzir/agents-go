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
endings, it does not deliver them. A task is also the one shape of background
work: a job of SEVERAL runs — a fixed step sequence, a loop until a check
passes — is a task too, its runs chained by the `Continue` hook (below), so
stop, retry, the restart sweep and the cap are written once.

## Wiring

```go
import "github.com/zzir/agents-go/agents/tasks"

mgr := tasks.New(tasks.Config{
	Store:    tasks.NewInMemoryStore(),   // or sessions.NewTaskStore(db)
	Sessions: repo,                       // a session.Repo — see "Deleting a session" for what its Delete owes
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

## Jobs of several runs

A task's work may span runs in sequence. Three fields on the task are the
host's own and opaque to the SDK: `Kind` names what sort of job it is,
`State` is where the job stands, and both are set at spawn
(`SpawnRequest.Kind`/`State`) and handed to the `Launcher` with every run
(`LaunchRequest.TaskID`/`Kind`/`State`). `Config.Continue` is asked when a
run of the CURRENT attempt completes or fails — never when it is cancelled: a
person's stop ends the task whatever the host would do next:

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

A `Continuation` WITHOUT an `Input` is an ending: the task ends — with the
run's own outcome, or failed with `Err` — and `State` is written in the same
`Finalize` as the ending (`Store.Finalize` takes it), so how the last run
ended is in the record and cannot disagree with the status. Returning `nil`
ends the task with `State` untouched.

The chain has a ceiling of its own: `Config.MaxContinuations` (default 50)
is how many further runs the hook may chain under one task since the spawn
or the last retry. A hook still asking for another run at the bound ends the
task failed — a loop no check ever ends stops costing runs — so the counter
in the example above is the host's own bound, not the only one.

A `Continuation` moves the task on: the Manager claims the transition with
`Store.Advance` — run id and State replaced together (a nil State keeps the
recorded one), only while the task is working on the run that just ended —
then launches the next run and reconciles with a stop that raced it, exactly
as a spawn does. The hook is only asked when the outcome names its run and
the row is still working on it: an outcome without a run id finalizes the
attempt the row names and never advances it, and a row paused for an
approval is finalized with the ending, not advanced. The attempt count is
untouched: a continuation is not a retry. A launch that fails ends the task
failed and reports it, the same as a retry whose run never started — and so
does a transition that cannot be written or that is not won (the row is
finalized on the run that just ended, never left working on it; `Finalize`'s
own predicate yields to whoever moved the row first). At the continuation
ceiling the task ends failed with the State it had, not the one the hook
returned. `Advance`
with the same run id on both sides rewrites State in place, which is how a
launcher records what it learns at launch (the run it is about to start) under
the CAS rather than beside it.

What the host gets from this: one lifecycle for every kind of background work.
Stop chases the current run; retry re-launches the current State — the launch
is marked `LaunchRequest.Retry`, its `Input` the retry prompt (why the last
attempt failed, resume from the progress made), and a job whose stage carries
its own instruction re-issues that instruction with it rather than leaving the
model to infer it from the transcript; the restart sweep fails the row at the
step it reached; `task_status` and the wake-up report the task, not its runs. `Task.Kind` is on `Info` and in `task_status`'s
output, so a model can tell one job from another; the SDK never branches on it.

## The tools

| | |
|---|---|
| `spawn_task` | Start a task; returns a `task_id` immediately |
| `task_status` | Read one, optionally waiting for it to finish; with no id, list the conversation's tasks |
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
the notification only carried a summary. Called with an empty `task_id` it
lists the conversation's tasks instead — newest first, status and summary per
line, each live one flagged "still working — do not redo its work" — which is
the way back to an id a compaction dropped; a listing settles no wake-up debt,
so a finish seen in it is still delivered. A host with jobs of its own kinds
says where one stands through `Config.DescribeState(kind, state) string` —
"step 2/3 (verify)" — and `task_status` shows it as `progress:` beside the
status, in the listing too; the SDK never reads `State` itself.

Four verbs are the whole surface, and a host keeps it that way even when it has
more kinds of background work than a plain task: `Manager.Tools` is
`SpawnTool` (spawn_task) followed by `TaskTools` (status, retry, stop), so a
host that starts jobs by name provides its OWN spawn tool from the public parts
— `Manager.Spawn`, `Manager.ModelHasResult` (settle the wake-up debt of a job
that finished before its tool call returned), `tasks.ToolResult` (the same
card) — and attaches `TaskTools` beside it. One vocabulary for the model:
start, look, retry, stop; what kind of thing was started is a parameter of the
first, not a fifth tool.

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

Waking the parent is yours — the Manager reports endings, it does not deliver
them ([spec §2.13](../reference/spec.md#213-background-tasks)). What the SDK
gives you is the report (`OnFinished`), the addressing (`Task.Inherit`, the
configuration snapshotted at spawn, and `Task.ParentRunID`, the spawning run)
and the formatter (`tasks.DefaultNotifyFormatter`), which renders the message
a woken parent reads:

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

A host whose delivery must survive crashes owes itself four rules
([spec §2.13](../reference/spec.md#213-background-tasks), "What a durable host
owes on top of the reports"):

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

The invariants the design exists for — identity separate from execution,
finalization and retry as compare-and-set transitions that name the attempt
they observed, a retry taking a concurrency slot, a cancellation reported as
delivered, a restart failing what it interrupted (`FailOrphans`) while
leaving a task paused on an approval alone, a half-finished spawn cleaning up
on a detached context, `input_required` not terminal, a continuation as a
run-bound CAS with state — are
[spec §2.13](../reference/spec.md#213-background-tasks). Each is a test in
`agents/tasks`.

## Deleting a session

`StopTree` stops the tasks; it does not delete anything, and neither does the
Manager. What must go with a deleted parent is the whole tree: its task rows
(a surviving one owes a wake-up to a conversation that no longer exists,
retried at every restart) and the hidden sessions its tasks ran in (a hidden
session has no listing of its own — anything left behind is unreachable
forever). **That cascade belongs to the `session.Repo` you pass, on `Delete`**,
and only a repo that holds both tables can do it:

| Repo | On `Delete` of a parent |
|---|---|
| `sessions.NewRepo(db)` (SQL) | Removes the session, its entries, its task rows in both roles, and every hidden session in the task tree, at any depth — one transaction |
| `session.NewInMemoryRepo()` | Removes the session and its entries only — it has no task table to look in. With `sessions.NewTaskStore(db)` beside it the rows and the child sessions are yours to remove: `StopTree`, then `ListByParent` and delete each child, then the rows |

The generation columns on the SQL task store are the second line, not the
first: a task row that survives a delete some other way is inert (it lists
nowhere, owes nothing, resolves no run), but it still exists. The cascade is
what stops it existing.

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
