# Human-in-the-loop

Some tools should not run without a human's sign-off. Mark a tool as requiring approval and the run **pauses before executing it**: you get the pending calls back, record approve/reject decisions, and resume — in the same process or, via serialization, in a completely different one.

## Requiring approval

```go
deleteRepo := agents.NewTool("delete_repo", "Permanently delete a repository.",
	func(ctx context.Context, tc *agents.ToolContext, args deleteArgs) (string, error) {
		return doDelete(args.Name)
	})
deleteRepo.NeedsApproval = true
```

Or decide per call from the arguments:

```go
deleteRepo.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, argsJSON, callID string) (bool, error) {
	return strings.Contains(argsJSON, `"prod"`), nil // only prod deletions need approval
}
```

## The interrupt → decide → resume loop

When the model requests an approval-gated tool, `Run` returns **without executing anything from that turn** — so nothing runs twice after resumption:

```go
res, err := agents.RunSync(ctx, agent, "delete the prod repo", opts)
if err != nil {
	log.Fatal(err)
}

for len(res.Interruptions) > 0 {
	for _, item := range res.Interruptions {
		fmt.Printf("approve %s(%s)? ", item.ToolName, item.Arguments)
		if askHuman() {
			res.State.Approve(item, false)
		} else {
			res.State.Reject(item, false, "denied by operator")
		}
	}
	res, err = agents.ResumeRunSync(ctx, res.State, opts)
	if err != nil {
		log.Fatal(err)
	}
}
fmt.Println(res.FinalOutputString())
```

- `RunResult.Interruptions` lists the pending calls (`ToolName`, `CallID`, raw `Arguments`).
- `RunResult.State` is the resumable `*RunState`; record decisions on it with `Approve` / `Reject`.
- `Reject`'s message (default: `"Tool execution was not approved."`) is sent to the model as the tool output so it can adapt.
- Pass `always=true` to `Approve`/`Reject` to apply the decision to **every future call of that tool** in the run.
- A resumed run can pause again (new approval-gated calls), hence the loop. The turn budget continues counting from where the run paused.
- Streams pause the same way: range to the end, read `Interruptions`/`State` off the `*RunCompletedEvent`'s result, and resume with `ResumeRun` (a stream) or `ResumeRunSync` (the result alone). `ResumeRunWith(ctx, state, opts, ctrl)` resumes on a `RunControl` you already hold instead of minting a new one, so a stop requested through it and input queued on it carry into the resumed run — what a middleware resuming in-chain does with the `Control` it received in `RunInput`. The resumed stream does not re-emit the paused turn's own items — it picks up with the approved tools' outputs and every later turn.
- Once a call has an explicit approve/reject decision, resuming does **not** re-invoke `NeedsApprovalFunc` for it — the checker's side effects and errors cannot re-fire for an already-resolved call.

## Pre-approval guardrails

By default a tool's [input guardrails](guardrails.md#placement-decides-scope) run only after approval, right before execution. `RunOptions.Exec.PreApprovalToolInputGuardrails` also runs them **before** the approval interruption is surfaced:

```go
res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{
	Exec: agents.ExecOptions{PreApprovalToolInputGuardrails: true},
	// ...
})
```

If a guardrail rejects the call, its message is returned to the model as the tool output — no approval request is emitted and the tool never runs, sparing the human a pointless round-trip. Calls that pass still re-run the same guardrails immediately before execution after approval, so time-sensitive checks are revalidated on resume.

## Approvals across processes

`RunState` serializes to JSON, so the approval can happen hours later in another process (a ticket queue, a Slack button, …):

```go
// Process A: pause and persist
data, _ := json.Marshal(res.State)
store.Save(runID, data)

// Process B: restore, decide, resume
state, err := agents.RunStateFromJSON(data, map[string]*agents.Agent{
	"assistant": assistant, // every agent that participated, by name
})
if err != nil { … }
state.Approve(state.Interruptions[0], false)
res, err := agents.ResumeRunSync(ctx, state, opts)
```

Because Go functions don't serialize, `RunStateFromJSON` needs a **registry** mapping agent names back to your `*Agent` values; the format round-trips within this SDK only. The state round-trips whole — input queued through `RunControl`, deferred tools already disclosed, the server-conversation cursor — so a cross-process resume behaves exactly like an in-process one ([spec §2.11b](../reference/spec.md#211b-run-control)). It also carries the original run's `MaxTurns`, which always wins over `RunOptions.Exec.MaxTurns` on resume (the default applies only when the state carries a zero); on a resumed result, `NewItems` carry their replayed input form — `Kind` and `Display()` survive, `Raw` is nil.

### Rebuilding transformed agents

The registry holds **your** `*Agent` values, so an agent that was transformed
at build time must be rebuilt the same way — `middleware.Plan.Apply`, tool
injection, whatever produced the agent the paused run was using. What a
rebuild does NOT restore is the transform's own progress: `Plan.Apply` returns
a fresh, **locked** `PlanPhase`, so a run that paused after its plan was
approved (an `exec_command` approval, say) would resume without its write
tools. Re-arm it from your own record before resuming — the durable answer to
"is this run past its plan phase" is whatever your `PlanPhase.OnUnlock` hook
wrote, and nothing else.

`RunState.Extra` is where such state rides the pause: a
`map[string]json.RawMessage` the SDK carries verbatim (never reads, never
writes), so what you must remember lives inside the state instead of in a
side channel next to it. Prefix your keys (`"plan:phase"`) to avoid
collisions.

```go
// Pausing: ride the phase state along.
res.State.Extra = map[string]json.RawMessage{
	"plan:unlocked": json.RawMessage(fmt.Sprintf("%t", phase.Executing())),
}
data, _ := json.Marshal(res.State)

// Resuming: rebuild, then re-arm before ResumeRun.
agent, phase := middleware.Plan{}.Apply(baseAgent)
state, _ := agents.RunStateFromJSON(data, map[string]*agents.Agent{agent.Name: agent})
if string(state.Extra["plan:unlocked"]) == "true" {
	_ = phase.Unlock()
}
```

`Extra` covers pause→resume, not crashes: a fact that must survive a crash mid-run — the moment the plan unlocked — needs your own durable write at that moment, which is what `PlanPhase.OnUnlock` is for ([spec §2.12](../reference/spec.md#212-middleware)).

## Sessions and approvals

With a [Session](sessions.md), the completed part of the turn is already saved when the run pauses and only the pending, output-less tool calls are held back until resume — pass the same `Session` in `ResumeRun`'s options ([Session semantics](sessions.md#session-semantics)).
